package main

import (
	"context"
	"fmt"
	"math"
	"sort"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/cheynewallace/tabby"
	"k8s.io/apimachinery/pkg/api/resource"
)

func main() {
	ec2api := ec2.New(session.Must(session.NewSessionWithOptions(session.Options{SharedConfigState: session.SharedConfigEnable})))
	ctx := context.Background()
	var instanceTypes []*ec2.InstanceTypeInfo
	ec2api.DescribeInstanceTypesPagesWithContext(ctx, &ec2.DescribeInstanceTypesInput{}, func(dito *ec2.DescribeInstanceTypesOutput, b bool) bool {
		instanceTypes = append(instanceTypes, dito.InstanceTypes...)
		return true
	})
	t := tabby.New()
	t.AddHeader("INSTANCE TYPE", "CPU OVERHEAD", "MEMORY (ENI) OVERHEAD", "MEMORY OVERHEAD", "DIFF")

	sort.Slice(instanceTypes, func(i, j int) bool {
		return aws.Int64Value(instanceTypes[i].MemoryInfo.SizeInMiB) < aws.Int64Value(instanceTypes[j].MemoryInfo.SizeInMiB)
	})
	for _, it := range instanceTypes {
		node := KubeOverhead{
			InstanceTypeInfo: it,
		}
		cpu := node.computeCPUOverhead()
		memENI := node.computeENIMemoryOverhead()
		mem := node.computeMemoryOverhead()
		t.AddLine(*it.InstanceType, cpu.String(), memENI.String(), mem.String(), memENI.AsApproximateFloat64()/mem.AsApproximateFloat64())
	}
	t.Print()
}

type KubeOverhead struct {
	*ec2.InstanceTypeInfo
}

func (i KubeOverhead) cpu() resource.Quantity {
	return resource.MustParse(fmt.Sprint(*i.VCpuInfo.DefaultVCpus))
}

func (i KubeOverhead) memory() resource.Quantity {
	return resource.MustParse(fmt.Sprintf("%dMi", *i.MemoryInfo.SizeInMiB))
}

func (i KubeOverhead) eniLimitedPods() int64 {
	return *i.NetworkInfo.MaximumNetworkInterfaces*(*i.NetworkInfo.Ipv4AddressesPerInterface-1) + 2
}

func (i KubeOverhead) computeCPUOverhead() resource.Quantity {
	cpuOverhead := *resource.NewMilliQuantity(
		100, // system-reserved
		resource.DecimalSI)
	// kube-reserved Computed from
	// https://github.com/bottlerocket-os/bottlerocket/pull/1388/files#diff-bba9e4e3e46203be2b12f22e0d654ebd270f0b478dd34f40c31d7aa695620f2fR611
	for _, cpuRange := range []struct {
		start      int64
		end        int64
		percentage float64
	}{
		{start: 0, end: 1000, percentage: 0.06},
		{start: 1000, end: 2000, percentage: 0.01},
		{start: 2000, end: 4000, percentage: 0.005},
		{start: 4000, end: 1 << 31, percentage: 0.0025},
	} {
		cpuSt := i.cpu()
		if cpu := cpuSt.MilliValue(); cpu >= cpuRange.start {
			r := float64(cpuRange.end - cpuRange.start)
			if cpu < cpuRange.end {
				r = float64(cpu - cpuRange.start)
			}
			cpuOverhead.Add(*resource.NewMilliQuantity(int64(r*cpuRange.percentage), resource.DecimalSI))
		}
	}
	return cpuOverhead
}

func (i KubeOverhead) computeENIMemoryOverhead() resource.Quantity {
	memory := i.memory()
	return resource.MustParse(fmt.Sprintf("%dMi",
		// vm-overhead
		(int64(math.Ceil(float64(memory.Value())*0.075/1024/1024)))+
			// kube-reserved
			((11*i.eniLimitedPods())+255)+
			// system-reserved
			100+
			// eviction threshold https://github.com/kubernetes/kubernetes/blob/ea0764452222146c47ec826977f49d7001b0ea8c/pkg/kubelet/apis/config/v1beta1/defaults_linux.go#L23
			100,
	))
}

func (i KubeOverhead) computeMemoryOverhead() resource.Quantity {
	//255 MiB of memory for machines with less than 1 GiB of memory
	// 25% of the first 4 GiB of memory
	// 20% of the next 4 GiB of memory (up to 8 GiB)
	// 10% of the next 8 GiB of memory (up to 16 GiB)
	// 6% of the next 112 GiB of memory (up to 128 GiB)
	// 2% of any memory above 128 GiB
	memory := i.memory()
	if memory.Cmp(resource.MustParse("1Gi")) <= 0 {
		// 200Mi for system-reserved and eviction threshold
		// + 255Mi
		return resource.MustParse("455Mi")
	}
	// 200Mi for system-reserved and eviction threshold
	memOverhead := resource.MustParse("200Mi")
	for _, memRange := range []struct {
		start      int64
		end        int64
		percentage float64
	}{
		{start: 0, end: 4096, percentage: 0.25},
		{start: 4096, end: 8192, percentage: 0.2},
		{start: 8192, end: 16_384, percentage: 0.1},
		{start: 16_384, end: 131_072, percentage: 0.06},
		{start: 131_072, end: 1 << 31, percentage: 0.02},
	} {
		memSt := i.memory()
		if mem := (memSt.Value() / 1024 / 1024); mem >= memRange.start {
			r := float64(memRange.end - memRange.start)
			if mem < memRange.end {
				r = float64(mem - memRange.start)
			}
			memOverhead.Add(resource.MustParse(fmt.Sprintf("%dMi", int64(r*memRange.percentage))))
		}
	}
	return memOverhead
}
