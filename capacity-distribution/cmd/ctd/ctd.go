/*
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"flag"
	"fmt"

	"github.com/aws/karpenter/pkg/utils/env"
	"github.com/bwagner5/karpenter-tools/capacity-distribution/pkg/distribution"
	"knative.dev/pkg/configmap"
	"knative.dev/pkg/controller"
	knativeinjection "knative.dev/pkg/injection"
	"knative.dev/pkg/injection/sharedmain"
	"knative.dev/pkg/signals"
	"knative.dev/pkg/system"
	"knative.dev/pkg/webhook"
	"knative.dev/pkg/webhook/certificates"
)

var options = MustParse()

func main() {
	config := knativeinjection.ParseAndGetRESTConfigOrDie()
	ctx := webhook.WithOptions(knativeinjection.WithNamespaceScope(signals.NewContext(), system.Namespace()), webhook.Options{
		Port:        options.WebhookPort,
		ServiceName: options.ServiceName,
		SecretName:  fmt.Sprintf("%s-cert", options.ServiceName),
	})

	// Controllers and webhook
	sharedmain.MainWithConfig(ctx, "webhook", config,
		certificates.NewController,
		newPodWebhook,
	)
}

func newPodWebhook(ctx context.Context, w configmap.Watcher) *controller.Impl {
	return distribution.NewDistributionController(ctx,
		"capacity-type-distribution.k8s.aws",
		"/distribute",
		fmt.Sprintf("%s-cert", options.ServiceName),
		"CustomPodScheduleStrategy")
}

func MustParse() Options {
	opts := Options{}
	flag.IntVar(&opts.MetricsPort, "metrics-port", env.WithDefaultInt("METRICS_PORT", 8080), "The port the metric endpoint binds to for operating metrics about the controller itself")
	flag.IntVar(&opts.HealthProbePort, "health-probe-port", env.WithDefaultInt("HEALTH_PROBE_PORT", 8081), "The port the health probe endpoint binds to for reporting controller health")
	flag.IntVar(&opts.WebhookPort, "port", 8443, "The port the webhook endpoint binds to for validation and mutation of resources")
	flag.StringVar(&opts.ServiceName, "service-name", "ctd", "The Service Name for the certificate generation")
	flag.Parse()
	return opts
}

// Options for running this binary
type Options struct {
	MetricsPort     int
	HealthProbePort int
	WebhookPort     int
	ServiceName     string
}
