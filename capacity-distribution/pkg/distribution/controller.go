package distribution

import (
	"context"
	"fmt"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	admissionlisters "k8s.io/client-go/listers/admissionregistration/v1"
	v1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	kubeclient "knative.dev/pkg/client/injection/kube/client"
	mwhinformer "knative.dev/pkg/client/injection/kube/informers/admissionregistration/v1/mutatingwebhookconfiguration"
	"knative.dev/pkg/controller"
	secretinformer "knative.dev/pkg/injection/clients/namespacedkube/informers/core/v1/secret"
	"knative.dev/pkg/kmp"
	"knative.dev/pkg/logging"
	"knative.dev/pkg/ptr"
	pkgreconciler "knative.dev/pkg/reconciler"
	"knative.dev/pkg/system"
	"knative.dev/pkg/webhook"
	certresources "knative.dev/pkg/webhook/certificates/resources"
	"knative.dev/pkg/webhook/json"
)

type DistributionController struct {
	webhook.StatelessAdmissionImpl
	pkgreconciler.LeaderAwareFuncs

	secretName    string
	key           types.NamespacedName
	path          string
	annotationKey string

	client       kubernetes.Interface
	podLister    v1.PodLister
	secretLister v1.SecretLister
	mwhLister    admissionlisters.MutatingWebhookConfigurationLister
}

type jsonPatch struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value string `json:"value"`
}

func NewDistributionController(ctx context.Context, name, path, secretName, annotationKey string) *controller.Impl {
	client := kubeclient.Get(ctx)
	secretInformer := secretinformer.Get(ctx)
	mwhInformer := mwhinformer.Get(ctx)
	factory := informers.NewSharedInformerFactory(client, 1*time.Minute)
	podInformer := factory.Core().V1().Pods()
	factory.Start(ctx.Done())
	key := types.NamespacedName{Name: name}
	hook := &DistributionController{
		LeaderAwareFuncs: pkgreconciler.LeaderAwareFuncs{
			// Have this reconciler enqueue our singleton whenever it becomes leader.
			PromoteFunc: func(bkt pkgreconciler.Bucket, enq func(pkgreconciler.Bucket, types.NamespacedName)) error {
				enq(bkt, key)
				return nil
			},
		},
		secretName:    secretName,
		key:           key,
		path:          path,
		client:        client,
		podLister:     podInformer.Lister(),
		secretLister:  secretInformer.Lister(),
		mwhLister:     mwhInformer.Lister(),
		annotationKey: annotationKey,
	}
	c := controller.NewContext(ctx, hook, controller.ControllerOptions{WorkQueueName: name, Logger: logging.FromContext(ctx).Named(name)})
	podInformer.Informer().AddEventHandler(cache.FilteringResourceEventHandler{
		FilterFunc: func(obj interface{}) bool {
			p := obj.(corev1.Pod)
			_, ok := p.ObjectMeta.Annotations[annotationKey]
			return ok
		},
		Handler: controller.HandleAll(c.Enqueue),
	})
	return c
}

func (d *DistributionController) Path() string {
	return d.path
}

func (d *DistributionController) Admit(ctx context.Context, req *admissionv1.AdmissionRequest) *admissionv1.AdmissionResponse {
	switch req.Operation {
	case admissionv1.Create:
		p := &corev1.Pod{}
		if err := json.Decode(req.Object.Raw, p, false); err != nil {
			logging.FromContext(ctx).Warnf("Unable to decode pod, %v", err)
			return &admissionv1.AdmissionResponse{
				Allowed: true,
			}
		}
		patch, err := d.distribute(ctx, p)
		if err != nil {
			logging.FromContext(ctx).Warnf("Unable to mutate pod, %v", err)
		}
		if patch != nil {
			patchJSON, err := json.Marshal(patch)
			if err != nil {
				logging.FromContext(ctx).Errorf("Unable to marshal json patch, %v", err)
			} else {
				return &admissionv1.AdmissionResponse{
					Allowed:   true,
					PatchType: (*admissionv1.PatchType)(ptr.String(string(admissionv1.PatchTypeJSONPatch))),
					Patch:     patchJSON,
				}
			}
		}
	default:
		logging.FromContext(ctx).Infof("Unhandled webhook operation, letting it through:  %s", req.Operation)
		return &admissionv1.AdmissionResponse{Allowed: true}
	}
	return &admissionv1.AdmissionResponse{
		Allowed: true,
	}
}

func (d *DistributionController) Reconcile(ctx context.Context, key string) error {
	caCert, err := d.getCACert(ctx)
	if err != nil {
		return err
	}
	if err := d.reconcileMutatingWebhook(ctx, caCert); err != nil {
		return err
	}
	fmt.Printf("KEY: %s", key)
	return nil
}

func (d *DistributionController) getCACert(ctx context.Context) ([]byte, error) {
	// Look up the webhook secret, and fetch the CA cert bundle.
	secret, err := d.secretLister.Secrets(system.Namespace()).Get(d.secretName)
	if err != nil {
		logging.FromContext(ctx).Errorf("Error fetching secret, %v", err)
		return nil, err
	}
	caCert, ok := secret.Data[certresources.CACert]
	if !ok {
		return nil, fmt.Errorf("secret %q is missing %q key", d.secretName, certresources.CACert)
	}
	return caCert, nil
}

func (d *DistributionController) reconcileMutatingWebhook(ctx context.Context, caCert []byte) error {
	logger := logging.FromContext(ctx)
	configuredWebhook, err := d.mwhLister.Get(d.key.Name)
	if err != nil {
		return fmt.Errorf("error retrieving webhook: %w", err)
	}

	current := configuredWebhook.DeepCopy()

	ns, err := d.client.CoreV1().Namespaces().Get(ctx, system.Namespace(), metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to fetch namespace: %w", err)
	}
	nsRef := *metav1.NewControllerRef(ns, corev1.SchemeGroupVersion.WithKind("Namespace"))
	current.OwnerReferences = []metav1.OwnerReference{nsRef}

	for i, wh := range current.Webhooks {
		if wh.Name != current.Name {
			continue
		}

		cur := &current.Webhooks[i]

		cur.NamespaceSelector = webhook.EnsureLabelSelectorExpressions(
			cur.NamespaceSelector,
			&metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{{
					Key:      "webhooks.knative.dev/exclude",
					Operator: metav1.LabelSelectorOpDoesNotExist,
				}},
			})

		cur.ClientConfig.CABundle = caCert
		if cur.ClientConfig.Service == nil {
			return fmt.Errorf("missing service reference for webhook: %s", wh.Name)
		}
		cur.ClientConfig.Service.Path = ptr.String(d.Path())
	}

	if ok, err := kmp.SafeEqual(configuredWebhook, current); err != nil {
		return fmt.Errorf("error diffing webhooks: %w", err)
	} else if !ok {
		logger.Info("Updating webhook")
		mwhclient := d.client.AdmissionregistrationV1().MutatingWebhookConfigurations()
		if _, err := mwhclient.Update(ctx, current, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("failed to update webhook: %w", err)
		}
	} else {
		logger.Info("Webhook is valid")
	}
	return nil
}
