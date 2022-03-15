package distribution

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"knative.dev/pkg/logging"
)

// example annotation:
// key=karpenter.sh/capacity-type:val=on-demand,base=1,weight=1:val=spot,weight=2

type distribution struct {
	// nodeSelectorKey is a node selector key to distribute over
	nodeSelectorKey string
	// baseKey is the nodeSelector value of the base distribution
	baseKey string
	dist    map[string]distLine
}

type distLine struct {
	val    string
	base   int
	weight int
}

func newDistribution(annotationVal string) (*distribution, error) {
	distribution := distribution{
		dist: map[string]distLine{},
	}
	baseRecorded := false
	keyRecorded := false
	for _, line := range strings.Split(annotationVal, ":") {
		distLine := distLine{}
		for _, stmt := range strings.Split(line, ",") {
			tokens := strings.Split(stmt, "=")
			if len(tokens) != 2 {
				return nil, fmt.Errorf("malformed distribution: %v", stmt)
			}
			var err error
			switch tokens[0] {
			case "key":
				if keyRecorded {
					return nil, fmt.Errorf("only one key can be specified, %v", stmt)
				}
				distribution.nodeSelectorKey = tokens[1]
				keyRecorded = true
				continue
			case "val":
				distLine.val = tokens[1]
				continue
			case "base":
				if baseRecorded {
					return nil, fmt.Errorf("only one base can be specified, %v", stmt)
				}
				distLine.base, err = strconv.Atoi(tokens[1])
				if err != nil {
					return nil, fmt.Errorf("cannot parse base as an int, %w", err)
				}
				baseRecorded = true
				continue
			case "weight":
				distLine.weight, err = strconv.Atoi(tokens[1])
				if err != nil {
					return nil, fmt.Errorf("cannot parse weight as an int, %w", err)
				}
				continue
			}
		}
		if distLine.base != 0 {
			distribution.baseKey = distLine.val
		}
		distribution.dist[distLine.val] = distLine
	}
	return &distribution, nil
}

func (d *DistributionController) distribute(ctx context.Context, pod *corev1.Pod) (*jsonPatch, error) {
	if pod.Namespace == "" {
		pod.Namespace = "default"
	}
	owningDeployment, err := d.deployment(ctx, pod)
	if err != nil {
		return nil, err
	}
	logging.FromContext(ctx).Infof("Got deployment for pod %v, %v", pod.Name, owningDeployment.Name)
	distributionVal, ok := owningDeployment.Annotations[d.annotationKey]
	if !ok {
		return nil, nil
	}
	dist, err := newDistribution(distributionVal)
	if err != nil {
		return nil, err
	}
	logging.FromContext(ctx).Infof("Got distribution for pod %v, %+v", pod.Name, dist.dist)
	selector, err := metav1.LabelSelectorAsSelector(owningDeployment.Spec.Selector)
	if err != nil {
		return nil, err
	}
	pods, err := d.podLister.Pods(pod.Namespace).List(selector)
	if err != nil {
		return nil, err
	}
	logging.FromContext(ctx).Infof("Got %d pods part of deployment %v", len(pods), owningDeployment.Name)

	// current pod distribution based on the label value
	currentDist := map[string]int{}
	for k := range dist.dist {
		currentDist[k] = 0
	}
	totalPods := 0
	for _, pod := range pods {
		val, ok := pod.Labels[dist.nodeSelectorKey]
		if !ok {
			continue
		}
		currentDist[val] = currentDist[val] + 1
		totalPods++
	}
	// replicas = 100
	// od = 75
	// spot = 24
	// Example: key=karpenter.sh/capacity-type:val=on-demand,base=1,weight=20:val=spot,weight=

	// spot => 0
	// od => 0
	for labelVal, quant := range currentDist {
		// if base req is not met
		if labelVal == dist.baseKey && quant < dist.dist[dist.baseKey].base {
			logging.FromContext(ctx).Infof("Created Patch for Base: %v=%v", dist.nodeSelectorKey, dist.baseKey)
			//mutate pod with base key
			return d.createPatch(dist.nodeSelectorKey, dist.baseKey), nil
		}
		distLine, ok := dist.dist[labelVal]
		if !ok {
			logging.FromContext(ctx).Infof("Created Patch for Base: %v=%v", dist.nodeSelectorKey, dist.baseKey)
			continue
		}
		if quant < (distLine.weight*totalPods)/100 {
			return d.createPatch(dist.nodeSelectorKey, labelVal), nil
		}
	}
	return nil, nil
}

func (d *DistributionController) createPatch(key, val string) *jsonPatch {
	return &jsonPatch{
		Op:    "add",
		Path:  fmt.Sprintf("/spec/nodeSelector/%s", key),
		Value: val,
	}
}

func (d *DistributionController) replicaSet(ctx context.Context, pod *corev1.Pod) (*appsv1.ReplicaSet, error) {
	for _, owner := range pod.OwnerReferences {
		if owner.Kind == "ReplicaSet" {
			replicaSet, err := d.client.AppsV1().ReplicaSets(pod.Namespace).Get(ctx, owner.Name, metav1.GetOptions{})
			if err != nil {
				return nil, fmt.Errorf("unable to query replicasets, %w", err)
			}
			return replicaSet, nil
		}
	}
	return nil, fmt.Errorf("unable to find replicasets for pod %v", pod.Name)
}

func (d *DistributionController) deployment(ctx context.Context, pod *corev1.Pod) (*appsv1.Deployment, error) {
	replicaSet, err := d.replicaSet(ctx, pod)
	if err != nil {
		return nil, err
	}
	for _, owner := range replicaSet.OwnerReferences {
		if owner.Kind == "Deployment" {
			deployment, err := d.client.AppsV1().Deployments(pod.Namespace).Get(ctx, owner.Name, metav1.GetOptions{})
			if err != nil {
				return nil, fmt.Errorf("unable to query deployments, %w", err)
			}
			return deployment, nil
		}
	}
	return nil, fmt.Errorf("could not find deployment for pod %v", pod.Name)
}
