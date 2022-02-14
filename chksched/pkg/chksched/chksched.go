package chksched

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ec2/ec2iface"
	"github.com/aws/karpenter/pkg/apis/provisioning/v1alpha5"
	"github.com/aws/karpenter/pkg/cloudprovider/aws/apis/v1alpha1"
	"github.com/google/uuid"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	LabelScenario = "scenario"
)

type Scenario struct {
	InstanceTypes   []string
	AMIFamilies     []string
	Timeout         time.Duration
	clientset       kubernetes.Interface
	kubeClient      client.Client
	ec2api          ec2iface.EC2API
	informerFactory informers.SharedInformerFactory
	podInformer     cache.SharedIndexInformer
	nodeInformer    cache.SharedIndexInformer
	id              string
}

type Result struct {
}

type KarpenterNode struct {
	*ec2.InstanceTypeInfo
}

func (k *KarpenterNode) MemoryOverheadInMiB() int64 {
	return (((*k.NetworkInfo.MaximumNetworkInterfaces * (*k.NetworkInfo.Ipv4AddressesPerInterface - 1)) + 2) * 11) + 255 + 300
}

func NewScenario(clientset kubernetes.Interface, kubeClient client.Client, ec2api ec2iface.EC2API, instanceTypes []string, amiFamilies []string) Scenario {
	id := fmt.Sprintf("chksched-scenario-%s", strings.ReplaceAll(uuid.New().String(), "-", ""))
	factory := informers.NewSharedInformerFactoryWithOptions(clientset, 1*time.Minute, informers.WithNamespace(id))
	podInformer := factory.Core().V1().Pods().Informer()
	nodeInformer := factory.Core().V1().Nodes().Informer()
	return Scenario{
		InstanceTypes:   instanceTypes,
		AMIFamilies:     amiFamilies,
		clientset:       clientset,
		kubeClient:      kubeClient,
		ec2api:          ec2api,
		informerFactory: factory,
		podInformer:     podInformer,
		nodeInformer:    nodeInformer,
		id:              id,
	}
}

func (s Scenario) Execute(ctx context.Context) (Result, error) {
	if _, err := s.clientset.CoreV1().Namespaces().Create(ctx, &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: s.id}}, metav1.CreateOptions{}); err != nil {
		return Result{}, err
	}
	defer s.tearDown(ctx)
	s.informerFactory.Start(ctx.Done())
	s.informerFactory.WaitForCacheSync(ctx.Done())

	instanceInfos, err := s.getInstanceTypeInfos(ctx)
	if err != nil {
		return Result{}, err
	}
	for _, knode := range instanceInfos {
		fmt.Printf("%s : %d MiB\n", *knode.InstanceType, knode.MemoryOverheadInMiB())
	}

	for _, knode := range instanceInfos {
		for _, amiFamily := range s.AMIFamilies {
			s.execTest(ctx, knode, amiFamily)
		}
	}

	return Result{}, nil
}

func (s Scenario) execTest(ctx context.Context, instanceType *KarpenterNode, amiFamily string) {
	if err := s.ensureProvisioner(ctx, amiFamily); err != nil {
		fmt.Println(err)
	}
}

func (s Scenario) ensureProvisioner(ctx context.Context, amiFamily string) error {
	provisioner := &v1alpha5.Provisioner{}
	if err := s.kubeClient.Get(ctx, types.NamespacedName{Name: s.provisionerName(amiFamily)}, provisioner); err != nil {
		if !errors.IsNotFound(err) {
			return err
		}
	}
	if provisioner.Name != "" {
		return nil
	}
	deadline, _ := ctx.Deadline()
	awsProvider := &v1alpha1.AWS{
		//AMIFamily: amiFamily,
		InstanceProfile:       "KarpenterNodeInstanceProfile-karpenter-demo",
		SecurityGroupSelector: map[string]string{"kubernetes.io/cluster/karpenter-demo": "*"},
		SubnetSelector:        map[string]string{"kubernetes.io/cluster/karpenter-demo": "*"},
	}
	awsProviderBytes, err := json.Marshal(awsProvider)
	if err != nil {
		return err
	}
	provisioner = &v1alpha5.Provisioner{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "karpenter.sh/v1alpha5",
			Kind:       "Provisioner",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:   s.provisionerName(amiFamily),
			Labels: map[string]string{LabelScenario: s.id},
		},
		Spec: v1alpha5.ProvisionerSpec{
			TTLSecondsAfterEmpty:   aws.Int64(30),
			TTLSecondsUntilExpired: aws.Int64(int64(time.Until(deadline).Seconds())),
			Constraints: v1alpha5.Constraints{
				Requirements: v1alpha5.Requirements{
					Requirements: []v1.NodeSelectorRequirement{
						{Key: v1.LabelArchStable, Operator: v1.NodeSelectorOpIn, Values: []string{"arm64", "amd64"}},
						{Key: v1alpha5.LabelCapacityType, Operator: v1.NodeSelectorOpIn, Values: []string{"on-demand"}},
					},
				},
				Provider: &runtime.RawExtension{Raw: awsProviderBytes},
			},
		},
	}
	if err := s.kubeClient.Create(ctx, provisioner); err != nil {
		return err
	}
	return nil
}

func (s Scenario) provisionerName(amiFamily string) string {
	return strings.ToLower(fmt.Sprintf("%s-%s", s.id, amiFamily))
}

func (s Scenario) getInstanceTypeInfos(ctx context.Context) ([]*KarpenterNode, error) {
	describeInput := ec2.DescribeInstanceTypesInput{}
	if len(s.InstanceTypes) != 0 {
		describeInput.InstanceTypes = aws.StringSlice(s.InstanceTypes)
	}
	instanceTypeInfos := []*KarpenterNode{}
	if err := s.ec2api.DescribeInstanceTypesPagesWithContext(ctx, &describeInput, func(output *ec2.DescribeInstanceTypesOutput, _ bool) bool {
		for _, it := range output.InstanceTypes {
			instanceTypeInfos = append(instanceTypeInfos, &KarpenterNode{InstanceTypeInfo: it})
		}
		return true
	}); err != nil {
		return nil, err
	}
	return instanceTypeInfos, nil
}

func (s Scenario) setup() {

}

func (s Scenario) tearDown(ctx context.Context) error {
	if err := s.clientset.CoreV1().Namespaces().Delete(ctx, s.id, metav1.DeleteOptions{}); err != nil {
		return err
	}
	provisioner := &v1alpha5.Provisioner{}
	if err := s.kubeClient.DeleteAllOf(ctx, provisioner, &client.MatchingLabels{LabelScenario: s.id}); err != nil {
		return err
	}
	return nil
}
