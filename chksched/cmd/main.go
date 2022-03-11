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
	"github.com/jedib0t/go-pretty/v6/table"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	defaultTimeout       = "12h"
	defaultAMIFamilies   = "AL2 Ubuntu Bottlerocket"
	defaultMaxConcurrent = 25
)

var (
	versionID = "dev"
)

type StartOptions struct {
	InstanceTypes []string
	AMIFamilies   []string
	Timeout       time.Duration
	MaxConcurrent int
	Kubeconfig    string
	Region        string
	Profile       string
	Keep          bool
}

type CleanOptions struct {
	ID         string
	Kubeconfig string
}

func main() {
	// Start CMD
	startCmd := flag.NewFlagSet("start", flag.ExitOnError)
	startOptions := StartOptions{}
	var instanceTypesInput, amiFamiliesInput, timeoutInput string
	startCmd.StringVar(&startOptions.Region, "region", "", "AWS Region to connect to")
	startCmd.StringVar(&startOptions.Profile, "profile", "", "AWS Profile to use")
	startCmd.StringVar(&instanceTypesInput, "instance-types", "", "list of instance types to check (example: --instance-types \"t3.micro m5.16xlarge\")")
	startCmd.StringVar(&amiFamiliesInput, "ami-families", defaultAMIFamilies, "list of AMI families to check (example: --ami-families \"AL2 Ubuntu Bottlerocket\")")
	startCmd.IntVar(&startOptions.MaxConcurrent, "max-concurrent", defaultMaxConcurrent, "number of concurrent tests being performed (roughly equates to number of nodes launched at one time)")
	startCmd.StringVar(&timeoutInput, "timeout", defaultTimeout, "how long the job is allowed to run before exiting early (go durations are support, i.e. 300s, 1h, etc)")
	startCmd.BoolVar(&startOptions.Keep, "keep", false, "keep resources like the job and namespace around after completing")
	if home := homedir.HomeDir(); home != "" {
		startCmd.StringVar(&startOptions.Kubeconfig, "kubeconfig", filepath.Join(home, ".kube", "config"), "(optional) absolute path to the kubeconfig file")
	} else {
		startCmd.StringVar(&startOptions.Kubeconfig, "kubeconfig", "", "absolute path to the kubeconfig file")
	}

	// Clean CMD
	cleanCmd := flag.NewFlagSet("clean", flag.ExitOnError)
	cleanOptions := CleanOptions{}
	cleanCmd.StringVar(&cleanOptions.ID, "id", defaultTimeout, "Scenario ID to clean up or \"all\" to clean-up all chksched scenarios")
	if home := homedir.HomeDir(); home != "" {
		cleanCmd.StringVar(&cleanOptions.Kubeconfig, "kubeconfig", filepath.Join(home, ".kube", "config"), "(optional) absolute path to the kubeconfig file")
	} else {
		cleanCmd.StringVar(&cleanOptions.Kubeconfig, "kubeconfig", "", "absolute path to the kubeconfig file")
	}

	switch os.Args[1] {
	case "start":
		startCmd.Parse(os.Args[2:])
		if instanceTypesInput != "" {
			startOptions.InstanceTypes = []string{}
			if spaceSplit := strings.Split(instanceTypesInput, " "); len(spaceSplit) > 1 {
				startOptions.InstanceTypes = append(startOptions.InstanceTypes, spaceSplit...)
			} else {
				startOptions.InstanceTypes = append(startOptions.InstanceTypes, strings.Split(instanceTypesInput, ",")...)
			}
		}
		startOptions.AMIFamilies = strings.Split(amiFamiliesInput, " ")

		var err error
		startOptions.Timeout, err = time.ParseDuration(timeoutInput)
		if err != nil {
			fmt.Printf("Invalid timeout duration specified (%s): %v", timeoutInput, err.Error())
			os.Exit(-1)
		}
		if err := Start(startOptions); err != nil {
			fmt.Printf("There was a problem running start: %v\n", err)
			os.Exit(-2)
		}
	case "clean":
		cleanCmd.Parse(os.Args[2:])
		if err := Clean(cleanOptions); err != nil {
			fmt.Printf("There was a problem running clean: %v\n", err)
			os.Exit(-2)
		}
	}
}

func Start(options StartOptions) error {
	clientset, kubeClient, err := k8sClients(options.Kubeconfig)
	if err != nil {
		return err
	}
	// AWS Session
	sess, err := aws.GetSession(&options.Region, &options.Profile)
	if err != nil {
		return err
	}

	// Start the actual chksched scenario
	ctx := context.Background()
	ctx, cancel := context.WithTimeout(ctx, options.Timeout)
	defer cancel()
	scenario := chksched.NewScenario(clientset, kubeClient, ec2.New(sess), options.MaxConcurrent, options.InstanceTypes, options.AMIFamilies)
	scenarioResult, err := scenario.Execute(ctx, options.Keep)
	if err != nil {
		return err
	}

	PrintTable(scenarioResult)
	return nil
}

func PrintTable(scenarioResult chksched.ScenarioResult) {
	t := table.NewWriter()
	t.SetOutputMirror(os.Stdout)
	t.AppendHeader(table.Row{"Instance Type", "Overhead Offset", "Percent Offset", "Pass", "EC2 Mem", "Overhead", "Message"})
	for _, amiResults := range scenarioResult {
		instanceTypeLabeled := false
		for _, results := range [][]chksched.Result{amiResults.AL2, amiResults.Bottlerocket, amiResults.Ubuntu} {
			instanceType := ""
			if !instanceTypeLabeled && len(results) > 0 {
				instanceType = results[0].InstanceType
				instanceTypeLabeled = true
			}
			for j, result := range results {
				diffFromOverhead := fmt.Sprintf("%s: %d MiB", result.AMIFamily, result.DifferenceFromOverhead)
				percentDiffFromOverhead := fmt.Sprintf("%s: %.2f", result.AMIFamily, result.PercentDiffFromOverhead)
				ec2TotalMem := fmt.Sprintf("%s: %d MiB", result.AMIFamily, result.EC2TotalMemory)
				karpOverhead := fmt.Sprintf("%s: %d MiB", result.AMIFamily, result.KarpenterOverhead)
				if result.Error != nil {
					t.AppendRows([]table.Row{{instanceType, diffFromOverhead, percentDiffFromOverhead, "❌ ERROR ❌", ec2TotalMem, karpOverhead, result.Error.Error()}})
					break
				}
				if result.Pass || j == len(results)-1 {
					pass := " ✅ "
					if !result.Pass {
						pass = " ❌ "
					}
					t.AppendRows([]table.Row{{instanceType, diffFromOverhead, percentDiffFromOverhead, pass, ec2TotalMem, karpOverhead, result.Message}})
					break
				}
			}
		}
		t.AppendSeparator()
	}
	t.Render()
}

func Clean(options CleanOptions) error {
	clientset, kubeClient, err := k8sClients(options.Kubeconfig)
	if err != nil {
		return err
	}
	ctx := context.Background()
	if err := chksched.CleanUpScenario(ctx, clientset, kubeClient, options.ID); err != nil {
		return err
	}
	fmt.Printf("Sucessfully cleaned up %s\n", options.ID)
	return nil
}

func k8sClients(kubeconfig string) (*kubernetes.Clientset, client.Client, error) {
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, nil, err
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, nil, err
	}

	scheme := runtime.NewScheme()
	clientgoscheme.AddToScheme(scheme)
	if err := apis.AddToScheme(scheme); err != nil {
		return nil, nil, err
	}
	kubeClient, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		return nil, nil, err
	}
	return clientset, kubeClient, nil
}
