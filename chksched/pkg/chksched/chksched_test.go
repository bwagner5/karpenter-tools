package chksched

import (
	"context"
	"fmt"
	"testing"
)

func TestGetInstanceTypeInfos(t *testing.T) {
	instanceTypes := []string{}
	for i := 0; i < 2; i++ {
		instanceTypes = append(instanceTypes, fmt.Sprint(i))
	}
	scenario := Scenario{
		InstanceTypes: instanceTypes,
	}
	kNodes, err := scenario.getInstanceTypeInfos(context.Background())
	if err != nil {
		t.FailNow()
	}
	if len(kNodes) != len(instanceTypes) {
		t.FailNow()
	}
	uniq := map[string]bool{}
	for _, knode := range kNodes {
		if _, ok := uniq[*knode.InstanceType]; ok {
			t.FailNow()
		}
		uniq[*knode.InstanceType] = true
	}
	t.FailNow()
}
