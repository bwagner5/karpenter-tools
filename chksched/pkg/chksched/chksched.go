package chksched

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ec2/ec2iface"
	"github.com/aws/karpenter/pkg/apis/provisioning/v1alpha5"
	"github.com/aws/karpenter/pkg/cloudprovider/aws/apis/v1alpha1"
	"github.com/google/uuid"
	"go.uber.org/multierr"
	batchv1 "k8s.io/api/batch/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"knative.dev/pkg/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	LabelScenario = "scenario"
	IDPrefix      = "chksched-scenario"
	OOMKilledCode = 137
)

type Scenario struct {
	InstanceTypes   []string
	AMIFamilies     []string
	Timeout         time.Duration
	MaxConcurrent   int
	clientset       kubernetes.Interface
	kubeClient      client.Client
	ec2api          ec2iface.EC2API
	informerFactory informers.SharedInformerFactory
	podInformer     cache.SharedIndexInformer
	jobInformer     cache.SharedIndexInformer
	nodeInformer    cache.SharedIndexInformer
	id              string
}

type ScenarioResult map[string]AMIResults

type AMIResults struct {
	AL2          []Result
	Ubuntu       []Result
	Bottlerocket []Result
}

type Result struct {
	InstanceType            string
	AMIFamily               string
	MemoryInMiB             int64
	DifferenceFromOverhead  int64
	PercentDiffFromOverhead float64
	Pass                    bool
	Message                 string
	Error                   error
	EC2TotalMemory          int64
	KarpenterOverhead       int64
}

type KarpenterNode struct {
	*ec2.InstanceTypeInfo
}

func (k *KarpenterNode) MemoryOverheadInMiB() int64 {
	percentageOverhead := int64(float64(*k.MemoryInfo.SizeInMiB) * .075) //int64(math.Log2(float64(*k.MemoryInfo.SizeInMiB)) * 500)
	return (((*k.NetworkInfo.MaximumNetworkInterfaces * (*k.NetworkInfo.Ipv4AddressesPerInterface - 1)) + 2) * 11) + 255 + 200 + percentageOverhead
}

func NewScenario(clientset kubernetes.Interface, kubeClient client.Client, ec2api ec2iface.EC2API, maxConcurrent int, instanceTypes []string, amiFamilies []string) Scenario {
	id := fmt.Sprintf("%s-%s", IDPrefix, strings.ReplaceAll(uuid.New().String(), "-", ""))
	factory := informers.NewSharedInformerFactoryWithOptions(clientset, 1*time.Minute, informers.WithNamespace(id))
	jobInformer := factory.Batch().V1().Jobs().Informer()
	podInformer := factory.Core().V1().Pods().Informer()
	nodeInformer := factory.Core().V1().Nodes().Informer()
	return Scenario{
		InstanceTypes:   instanceTypes,
		AMIFamilies:     amiFamilies,
		MaxConcurrent:   maxConcurrent,
		clientset:       clientset,
		kubeClient:      kubeClient,
		ec2api:          ec2api,
		informerFactory: factory,
		podInformer:     podInformer,
		jobInformer:     jobInformer,
		nodeInformer:    nodeInformer,
		id:              id,
	}
}

func CleanUpScenario(ctx context.Context, clientset kubernetes.Interface, kubeClient client.Client, id string) error {
	ids := []string{id}
	if id == "all" {
		ids = []string{}
		nsList, err := clientset.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
		if err != nil {
			return err
		}
		for _, ns := range nsList.Items {
			if strings.HasPrefix(ns.Name, IDPrefix) {
				ids = append(ids, ns.Name)
			}
		}
	}
	var errs error
	for _, sid := range ids {
		log.Printf("Cleaning up %s\n", sid)
		errs = multierr.Append(errs, Scenario{
			id:         sid,
			kubeClient: kubeClient,
			clientset:  clientset,
		}.TearDown())
	}
	return errs
}

func (s Scenario) Execute(ctx context.Context, keepResources bool) (ScenarioResult, error) {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

	if _, err := s.clientset.CoreV1().Namespaces().Create(ctx, &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: s.id}}, metav1.CreateOptions{}); err != nil {
		return nil, err
	}
	if !keepResources {
		defer s.TearDown()
	}
	s.informerFactory.Start(ctx.Done())
	s.informerFactory.WaitForCacheSync(ctx.Done())

	instanceInfos, err := s.getInstanceTypeInfos(ctx)
	if err != nil {
		return nil, err
	}

	resultChans := []chan []Result{}
	resultCount := 0
	go func() {
		for _, knode := range instanceInfos {
			for _, amiFamily := range s.AMIFamilies {
				resultChan := make(chan []Result)
				resultChans = append(resultChans, resultChan)
				go s.execTest(ctx, resultChan, knode, amiFamily)
				for len(resultChans)-resultCount >= s.MaxConcurrent {
					time.Sleep(1 * time.Second)
				}
			}
		}
	}()

	results := []Result{}
	for resultCount < len(instanceInfos)*len(s.AMIFamilies) {
		for _, resultChan := range resultChans {
			select {
			case result := <-resultChan:
				results = append(results, result...)
				resultCount++
				if len(result) > 0 {
					log.Printf("[%d/%d] Received %s result for %s - %s", resultCount, len(instanceInfos)*len(s.AMIFamilies), resultsIcon(results), result[0].InstanceType, result[0].AMIFamily)
				}
			case sig := <-sigs:
				log.Printf("Received %s, shutting down", sig)
				return ResultsToScenarioResult(results), nil
			case <-ctx.Done():
				log.Printf("User configured timeout reached, shutting down")
				return ResultsToScenarioResult(results), nil
			default:
			}
		}
	}
	return ResultsToScenarioResult(results), nil
}

func resultsIcon(results []Result) string {
	for _, result := range results {
		if result.Pass {
			return "✅"
		}
	}
	return "❌"
}

func ResultsToScenarioResult(results []Result) ScenarioResult {
	scenarioResult := ScenarioResult{}
	for _, result := range results {
		switch result.AMIFamily {
		case "AL2":
			if amiResult, ok := scenarioResult[result.InstanceType]; ok {
				amiResult.AL2 = append(amiResult.AL2, result)
				sort.Slice(amiResult.AL2, func(i, j int) bool {
					return amiResult.AL2[i].DifferenceFromOverhead < amiResult.AL2[j].DifferenceFromOverhead
				})
				scenarioResult[result.InstanceType] = amiResult
			} else {
				scenarioResult[result.InstanceType] = AMIResults{AL2: []Result{result}}
			}
		case "Ubuntu":
			if amiResult, ok := scenarioResult[result.InstanceType]; ok {
				amiResult.Ubuntu = append(amiResult.Ubuntu, result)
				sort.Slice(amiResult.Ubuntu, func(i, j int) bool {
					return amiResult.Ubuntu[i].DifferenceFromOverhead < amiResult.Ubuntu[j].DifferenceFromOverhead
				})
				scenarioResult[result.InstanceType] = amiResult
			} else {
				scenarioResult[result.InstanceType] = AMIResults{Ubuntu: []Result{result}}
			}
		case "Bottlerocket":
			if amiResult, ok := scenarioResult[result.InstanceType]; ok {
				amiResult.Bottlerocket = append(amiResult.Bottlerocket, result)
				sort.Slice(amiResult.Bottlerocket, func(i, j int) bool {
					return amiResult.Bottlerocket[i].DifferenceFromOverhead < amiResult.Bottlerocket[j].DifferenceFromOverhead
				})
				scenarioResult[result.InstanceType] = amiResult
			} else {
				scenarioResult[result.InstanceType] = AMIResults{Bottlerocket: []Result{result}}
			}
		}
	}
	return scenarioResult
}

func NewResult(instanceType *KarpenterNode, amiFamily string, memoryInMiB int64) Result {
	memoryRequest := *(instanceType.MemoryInfo.SizeInMiB) - instanceType.MemoryOverheadInMiB()
	return Result{
		InstanceType:            *instanceType.InstanceType,
		AMIFamily:               amiFamily,
		MemoryInMiB:             memoryInMiB,
		DifferenceFromOverhead:  memoryRequest - memoryInMiB,
		PercentDiffFromOverhead: float64(memoryInMiB) / float64(memoryRequest),
		EC2TotalMemory:          *instanceType.MemoryInfo.SizeInMiB,
		KarpenterOverhead:       instanceType.MemoryOverheadInMiB(),
	}
}

func (s Scenario) execTest(ctx context.Context, finish chan []Result, instanceType *KarpenterNode, amiFamily string) {
	memoryRequest := *(instanceType.MemoryInfo.SizeInMiB) - instanceType.MemoryOverheadInMiB()
	results := []Result{}
	for _, memoryInMiB := range []int64{memoryRequest, memoryRequest - 100, memoryRequest - 200, memoryRequest - 400, memoryRequest - 800, memoryRequest - 1600, memoryRequest - 3200, memoryRequest - int64(float64(memoryRequest)*0.95), memoryRequest - int64(float64(memoryRequest)*0.925)} {
		if memoryInMiB <= 0 {
			finish <- results
			close(finish)
			return
		}
		errResult := NewResult(instanceType, amiFamily, memoryInMiB)
		if err := s.ensureProvisioner(ctx, instanceType, amiFamily, memoryInMiB); err != nil {
			errResult.Error = err
			finish <- append(results, errResult)
			return
		}
		if err := s.ensureStressJob(ctx, instanceType, amiFamily, memoryInMiB); err != nil {
			errResult.Error = err
			finish <- append(results, errResult)
			return
		}
		resultChan := make(chan Result)
		s.nodeInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
			UpdateFunc: func(_ interface{}, obj interface{}) {
				s.checkNodeForMemoryIssues(ctx, resultChan, obj.(*v1.Node), instanceType, amiFamily, memoryInMiB)
			},
			DeleteFunc: func(obj interface{}) {
				s.checkNodeForMemoryIssues(ctx, resultChan, obj.(*v1.Node), instanceType, amiFamily, memoryInMiB)
			},
		})
		s.jobInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
			UpdateFunc: func(_, obj interface{}) {
				s.checkJobForMemoryIssues(ctx, resultChan, obj.(*batchv1.Job), instanceType, amiFamily, memoryInMiB)
			},
			DeleteFunc: func(obj interface{}) {
				s.checkJobForMemoryIssues(ctx, resultChan, obj.(*batchv1.Job), instanceType, amiFamily, memoryInMiB)
			},
		})
		s.podInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
			UpdateFunc: func(_, obj interface{}) {
				s.checkPodForMemoryIssues(ctx, resultChan, obj.(*v1.Pod), instanceType, amiFamily, memoryInMiB)
			},
			DeleteFunc: func(obj interface{}) {
				s.checkPodForMemoryIssues(ctx, resultChan, obj.(*v1.Pod), instanceType, amiFamily, memoryInMiB)
			},
		})
		log.Printf("Testing %s %s w/ %d MiB of memory (%d MiB less than default)\n", *instanceType.InstanceType, amiFamily, memoryInMiB, memoryRequest-memoryInMiB)
		result := <-resultChan
		results = append(results, result)
		if result.Pass {
			break
		}
		go func(mem int64) {
			time.Sleep(10 * time.Second)
			if err := s.deleteStressJob(ctx, instanceType, amiFamily, mem); err != nil {
				log.Println(err)
			}
			time.Sleep(10 * time.Second)
			if err := s.deleteNodes(ctx, s.provisionerName(instanceType, amiFamily, mem)); err != nil {
				log.Println(err)
			}
			time.Sleep(10 * time.Second)
			if err := s.deleteProvisioner(ctx, instanceType, amiFamily, mem); err != nil {
				log.Println(err)
			}
		}(memoryInMiB)
	}
	finish <- results
}

func (s Scenario) deleteProvisioner(ctx context.Context, instanceType *KarpenterNode, amiFamily string, memoryInMiB int64) error {
	provisioner := v1alpha5.Provisioner{ObjectMeta: metav1.ObjectMeta{Name: s.provisionerName(instanceType, amiFamily, memoryInMiB)}}
	return s.kubeClient.Delete(ctx, &provisioner, &client.DeleteOptions{})
}

func (s Scenario) deleteStressJob(ctx context.Context, instanceType *KarpenterNode, amiFamily string, memoryInMiB int64) error {
	return s.clientset.BatchV1().Jobs(s.id).Delete(ctx, s.jobName(instanceType, amiFamily, memoryInMiB), metav1.DeleteOptions{})
}

func (s Scenario) deleteNodes(ctx context.Context, provisionerName string) error {
	return s.clientset.CoreV1().Nodes().DeleteCollection(ctx, metav1.DeleteOptions{}, metav1.ListOptions{LabelSelector: fmt.Sprintf("%s=%s", v1alpha5.ProvisionerNameLabelKey, provisionerName)})
}

func (s Scenario) checkNodeForMemoryIssues(ctx context.Context, resultChan chan Result, node *v1.Node, instanceType *KarpenterNode, amiFamily string, memoryInMiB int64) {
	if provisionerName, ok := node.Labels[v1alpha5.ProvisionerNameLabelKey]; !ok || provisionerName != s.provisionerName(instanceType, amiFamily, memoryInMiB) {
		return
	}
	ready := false
	for _, condition := range node.Status.Conditions {
		if condition.Type == v1.NodeReady && condition.Status == v1.ConditionTrue {
			ready = true
		}
	}
	if !ready {
		return
	}
	for _, condition := range node.Status.Conditions {
		if v1.NodeMemoryPressure == condition.Type && condition.Status == v1.ConditionTrue {
			result := NewResult(instanceType, amiFamily, memoryInMiB)
			result.Pass = false
			result.Message = fmt.Sprintf("Node (%s) has memory pressure", node.Name)
			resultChan <- result
		}
	}
}

func (s Scenario) checkJobForMemoryIssues(ctx context.Context, resultChan chan Result, job *batchv1.Job, instanceType *KarpenterNode, amiFamily string, memoryInMiB int64) {
	if job.Name != s.jobName(instanceType, amiFamily, memoryInMiB) {
		return
	}
	result := NewResult(instanceType, amiFamily, memoryInMiB)
	if job.Status.Succeeded > 0 {
		result.Pass = true
		result.Message = fmt.Sprintf("Job passed memory tests w/ %d failures!", job.Status.Failed)
		resultChan <- result
		return
	} else if job.Status.Failed > 0 {
		result.Pass = false
		result.Message = fmt.Sprintf("Job (%s) has failed", job.Name)
		resultChan <- result
		return
	} else if job.Status.UncountedTerminatedPods != nil && len(job.Status.UncountedTerminatedPods.Failed) > 0 {
		result.Pass = false
		result.Message = fmt.Sprintf("Job (%s) has uncounted terminated failed pods", job.Name)
		resultChan <- result
		return
	}

	for _, condition := range job.Status.Conditions {
		if condition.Type == batchv1.JobFailed {
			result.Pass = false
			result.Message = fmt.Sprintf("Job (%s) has failed condition", job.Name)
			resultChan <- result
			return
		}
	}
}

func (s Scenario) checkPodForMemoryIssues(ctx context.Context, resultChan chan Result, pod *v1.Pod, instanceType *KarpenterNode, amiFamily string, memoryInMiB int64) {
	if pod.Labels["job-name"] != s.jobName(instanceType, amiFamily, memoryInMiB) {
		return
	}
	result := NewResult(instanceType, amiFamily, memoryInMiB)
	if pod.Status.Phase == v1.PodFailed && pod.Status.Reason == "OutOfmemory" {
		result.Pass = false
		result.Message = pod.Status.Message
		resultChan <- result
		return
	}
	for _, status := range pod.Status.ContainerStatuses {
		if status.LastTerminationState.Terminated != nil && status.LastTerminationState.Terminated.ExitCode == OOMKilledCode {
			result.Pass = false
			result.Message = fmt.Sprintf("Pod (%s) has been OOMKilled", pod.Name)
			resultChan <- result
		}
	}
}

func (s Scenario) ensureStressJob(ctx context.Context, instanceType *KarpenterNode, amiFamily string, memoryInMiB int64) error {
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:   s.jobName(instanceType, amiFamily, memoryInMiB),
			Labels: map[string]string{LabelScenario: s.id},
		},
		Spec: batchv1.JobSpec{
			Parallelism:             ptr.Int32(1),
			Completions:             ptr.Int32(1),
			BackoffLimit:            ptr.Int32(1),
			ActiveDeadlineSeconds:   ptr.Int64(300),
			TTLSecondsAfterFinished: ptr.Int32(600),
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{LabelScenario: s.id},
				},
				Spec: v1.PodSpec{
					RestartPolicy:                 v1.RestartPolicyNever,
					TerminationGracePeriodSeconds: aws.Int64(1),
					NodeSelector: map[string]string{
						v1alpha5.ProvisionerNameLabelKey: s.provisionerName(instanceType, amiFamily, memoryInMiB),
						v1alpha5.LabelCapacityType:       "on-demand",
						v1.LabelInstanceTypeStable:       *instanceType.InstanceType,
					},
					Containers: []v1.Container{
						{
							Name:  "stress",
							Image: "public.ecr.aws/f3m9g5x6/stress-ng:v0.10.10",
							Resources: v1.ResourceRequirements{
								Requests: v1.ResourceList{
									v1.ResourceCPU:    resource.MustParse("100m"),
									v1.ResourceMemory: resource.MustParse(fmt.Sprintf("%dMi", memoryInMiB)),
								},
							},
							Args: []string{
								"--vm", "1",
								"--vm-hang", "1", // 1s
								"--vm-bytes", fmt.Sprintf("%dM", memoryInMiB),
								"--backoff", "100", // 100us
								"--timeout", "10s",
								"--verbose",
							},
						},
					},
				},
			},
		},
	}

	if _, err := s.clientset.BatchV1().Jobs(s.id).Create(ctx, job, metav1.CreateOptions{}); err != nil {
		if errors.IsAlreadyExists(err) {
			return nil
		}
		return err
	}
	return nil
}

func (s Scenario) ensureProvisioner(ctx context.Context, instanceType *KarpenterNode, amiFamily string, memoryInMiB int64) error {
	provisioner := &v1alpha5.Provisioner{}
	if err := s.kubeClient.Get(ctx, types.NamespacedName{Name: s.provisionerName(instanceType, amiFamily, memoryInMiB)}, provisioner); err != nil {
		if !errors.IsNotFound(err) {
			return err
		}
	}
	if provisioner.Name != "" {
		return nil
	}
	deadline, _ := ctx.Deadline()
	awsProvider := &v1alpha1.AWS{
		InstanceProfile:       aws.String("KarpenterNodeInstanceProfile-karpenter-demo"),
		AMIFamily:             aws.String(amiFamily),
		SecurityGroupSelector: map[string]string{"kubernetes.io/cluster/karpenter-demo": "*"},
		SubnetSelector:        map[string]string{"kubernetes.io/cluster/karpenter-demo": "*"},
	}
	awsProviderBytes, err := json.Marshal(awsProvider)
	if err != nil {
		return err
	}
	provisioner = &v1alpha5.Provisioner{
		ObjectMeta: metav1.ObjectMeta{
			Name:   s.provisionerName(instanceType, amiFamily, memoryInMiB),
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
				Labels:   map[string]string{LabelScenario: s.id},
				Provider: &runtime.RawExtension{Raw: awsProviderBytes},
			},
		},
	}
	if err := s.kubeClient.Create(ctx, provisioner); err != nil {
		return err
	}
	return nil
}

func (s Scenario) provisionerName(instanceType *KarpenterNode, amiFamily string, memoryInMiB int64) string {
	return strings.ToLower(fmt.Sprintf("%s-%s-%dM", strings.ReplaceAll(*instanceType.InstanceType, ".", "-"), amiFamily, memoryInMiB))
}

func (s Scenario) jobName(instanceType *KarpenterNode, amiFamily string, memoryInMiB int64) string {
	return strings.ToLower(fmt.Sprintf("stress-%s-%s-%dM", *instanceType.InstanceType, amiFamily, memoryInMiB))
}

func (s Scenario) getInstanceTypeInfos(ctx context.Context) ([]*KarpenterNode, error) {
	instanceTypeInfos := []*KarpenterNode{}
	for i := 0; len(instanceTypeInfos) < len(s.InstanceTypes) || len(s.InstanceTypes) == 0; i++ {
		describeInput := ec2.DescribeInstanceTypesInput{}
		if len(s.InstanceTypes) != 0 {
			describeInput.InstanceTypes = aws.StringSlice(s.InstanceTypes[i*100 : int(math.Min(float64(100*(i+1)), float64(len(s.InstanceTypes))))])
		}
		if err := s.ec2api.DescribeInstanceTypesPagesWithContext(ctx, &describeInput, func(output *ec2.DescribeInstanceTypesOutput, _ bool) bool {
			for _, it := range output.InstanceTypes {
				instanceTypeInfos = append(instanceTypeInfos, &KarpenterNode{InstanceTypeInfo: it})
			}
			return true
		}); err != nil {
			return nil, err
		}
		if len(s.InstanceTypes) == 0 {
			break
		}
	}
	return instanceTypeInfos, nil
}

func (s Scenario) TearDown() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	var errs error
	scenarioIDSelector := fmt.Sprintf("%s=%s", LabelScenario, s.id)
	if err := s.clientset.BatchV1().Jobs(s.id).DeleteCollection(ctx, metav1.DeleteOptions{}, metav1.ListOptions{LabelSelector: scenarioIDSelector}); err != nil {
		log.Println(err)
		errs = multierr.Append(errs, err)
	}
	if err := s.clientset.CoreV1().Nodes().DeleteCollection(ctx, metav1.DeleteOptions{}, metav1.ListOptions{LabelSelector: scenarioIDSelector}); err != nil {
		log.Println(err)
		errs = multierr.Append(errs, err)
	}
	if err := s.clientset.CoreV1().Namespaces().Delete(ctx, s.id, metav1.DeleteOptions{}); err != nil {
		log.Println(err)
		errs = multierr.Append(errs, err)
	}
	time.Sleep(10 * time.Second)
	provisioner := &v1alpha5.Provisioner{}
	if err := s.kubeClient.DeleteAllOf(ctx, provisioner, &client.MatchingLabels{LabelScenario: s.id}); err != nil {
		log.Println(err)
		errs = multierr.Append(errs, err)
	}
	return errs
}
