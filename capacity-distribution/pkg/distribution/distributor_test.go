package distribution

import "testing"

func TestNewDistribution(t *testing.T) {
	dist, err := newDistribution("key=karpenter.sh/capacity-type:val=on-demand,base=1,weight=20:val=spot,weight=80")
	if err != nil {
		t.FailNow()
	}
	if dist.baseKey != "on-demand" {
		t.FailNow()
	}
	if dist.nodeSelectorKey != "karpenter.sh/capacity-type" {
		t.FailNow()
	}
	if val, ok := dist.dist["on-demand"]; !ok || val.weight != 20 {
		t.FailNow()
	}
	if val, ok := dist.dist["spot"]; !ok || val.weight != 80 {
		t.FailNow()
	}
}
