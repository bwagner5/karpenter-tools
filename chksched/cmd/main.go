package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/karpenter/pkg/apis"
	"github.com/bwagner5/chk-sched/pkg/aws"
	"github.com/bwagner5/chk-sched/pkg/chksched"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	defaultTimeout     = "20m"
	defaultAMIFamilies = "AL2 Ubuntu Bottlerocket"
)

type options struct {
	InstanceTypes []string
	AMIFamilies   []string
	Timeout       time.Duration
	Kubeconfig    string
	Region        string
	Profile       string
}

func main() {

	// Flag parsing
	options := options{}
	var instanceTypesInput, amiFamiliesInput, timeoutInput string
	flag.StringVar(&options.Region, "region", "", "AWS Region to connect to")
	flag.StringVar(&options.Profile, "profile", "", "AWS Profile to use")
	flag.StringVar(&instanceTypesInput, "instance-types", "", "list of instance types to check (example: --instance-types \"t3.micro m5.16xlarge\")")
	flag.StringVar(&amiFamiliesInput, "ami-families", defaultAMIFamilies, "list of AMI families to check (example: --ami-families \"AL2 Ubuntu Bottlerocket\")")
	flag.StringVar(&timeoutInput, "timeout", defaultTimeout, "how long the job is allowed to run before exiting early (go durations are support, i.e. 300s, 1h, etc)")
	if home := homedir.HomeDir(); home != "" {
		flag.StringVar(&options.Kubeconfig, "kubeconfig", filepath.Join(home, ".kube", "config"), "(optional) absolute path to the kubeconfig file")
	} else {
		flag.StringVar(&options.Kubeconfig, "kubeconfig", "", "absolute path to the kubeconfig file")
	}
	flag.Parse()
	if instanceTypesInput != "" {
		options.InstanceTypes = strings.Split(instanceTypesInput, " ")
	}
	options.AMIFamilies = strings.Split(amiFamiliesInput, " ")

	var err error
	options.Timeout, err = time.ParseDuration(timeoutInput)
	if err != nil {
		fmt.Printf("Invalid timeout duration specified (%s): %v", timeoutInput, err.Error())
		os.Exit(-1)
	}

	// Kubernetes cluster client
	config, err := clientcmd.BuildConfigFromFlags("", options.Kubeconfig)
	if err != nil {
		fmt.Printf("Unable to find a kubeconfig: %v", err.Error())
		os.Exit(-1)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		fmt.Println(err.Error())
		os.Exit(-1)
	}

	scheme := runtime.NewScheme()
	clientgoscheme.AddToScheme(scheme)
	if err := apis.AddToScheme(scheme); err != nil {
		fmt.Println(err.Error())
		os.Exit(-1)
	}
	kubeClient, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		fmt.Println(err.Error())
		os.Exit(-1)
	}

	// AWS Session
	sess, err := aws.GetSession(&options.Region, &options.Profile)
	if err != nil {
		fmt.Println(err)
	}

	// Start the actual chksched scenario
	fmt.Println(options.Timeout)
	ctx := context.Background()
	ctx, cancel := context.WithTimeout(ctx, options.Timeout)
	defer cancel()
	scenario := chksched.NewScenario(clientset, kubeClient, ec2.New(sess), options.InstanceTypes, options.AMIFamilies)
	result, err := scenario.Execute(ctx)
	if err != nil {
		fmt.Printf("There was an error executing the chksched scenario: %v", err.Error())
		os.Exit(-2)
	}
	fmt.Printf("Results: \n%s", result)
}
