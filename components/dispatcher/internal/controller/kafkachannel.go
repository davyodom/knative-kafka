package controller

import (
	"context"
	"fmt"
	kafkav1alpha1 "github.com/kyma-incubator/knative-kafka/components/controller/pkg/apis/knativekafka/v1alpha1"
	"github.com/kyma-incubator/knative-kafka/components/controller/pkg/client/clientset/versioned"
	"github.com/kyma-incubator/knative-kafka/components/controller/pkg/client/clientset/versioned/scheme"
	"github.com/kyma-incubator/knative-kafka/components/controller/pkg/client/informers/externalversions/knativekafka/v1alpha1"
	listers "github.com/kyma-incubator/knative-kafka/components/controller/pkg/client/listers/knativekafka/v1alpha1"
	"github.com/kyma-incubator/knative-kafka/components/dispatcher/internal/dispatcher"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	eventingduck "knative.dev/eventing/pkg/apis/duck/v1beta1"
	"knative.dev/pkg/controller"
	"knative.dev/pkg/logging"
	"reflect"
)

const (
	// ReconcilerName is the name of the reconciler.
	ReconcilerName = "KafkaChannels"

	// corev1.Events emitted
	channelReconciled         = "ChannelReconciled"
	channelReconcileFailed    = "ChannelReconcileFailed"
	channelUpdateStatusFailed = "ChannelUpdateStatusFailed"
)

// Reconciler reconciles KafkaChannels.
type Reconciler struct {
	dispatcher           *dispatcher.Dispatcher
	Logger               *zap.Logger
	kafkachannelInformer cache.SharedIndexInformer
	kafkachannelLister   listers.KafkaChannelLister
	impl                 *controller.Impl
	Recorder             record.EventRecorder
	KafkaClientSet       versioned.Interface
}

var _ controller.Reconciler = Reconciler{}

// NewController initializes the controller and is called by the generated code.
// Registers event handlers to enqueue events.
func NewController(logger *zap.Logger, dispatcher *dispatcher.Dispatcher, kafkachannelInformer v1alpha1.KafkaChannelInformer, kubeClient kubernetes.Interface, kafkaClientSet versioned.Interface, stopChannel <-chan struct{}) *controller.Impl {

	r := &Reconciler{
		Logger:               logger,
		dispatcher:           dispatcher,
		kafkachannelInformer: kafkachannelInformer.Informer(),
		kafkachannelLister:   kafkachannelInformer.Lister(),
		KafkaClientSet:       kafkaClientSet,
	}
	r.impl = controller.NewImpl(r, r.Logger.Sugar(), ReconcilerName)

	r.Logger.Info("Setting Up Event Handlers")

	// Watch for kafka channels.
	kafkachannelInformer.Informer().AddEventHandler(controller.HandleAll(r.impl.Enqueue))
	logger.Debug("Creating event broadcaster")
	eventBroadcaster := record.NewBroadcaster()
	watches := []watch.Interface{
		eventBroadcaster.StartLogging(logger.Sugar().Named("event-broadcaster").Infof),
		eventBroadcaster.StartRecordingToSink(
			&typedcorev1.EventSinkImpl{Interface: kubeClient.CoreV1().Events("")}),
	}
	r.Recorder = eventBroadcaster.NewRecorder(
		scheme.Scheme, corev1.EventSource{Component: ReconcilerName})
	go func() {
		<-stopChannel
		for _, w := range watches {
			w.Stop()
		}
	}()

	return r.impl
}

func (r Reconciler) Reconcile(ctx context.Context, key string) error {

	r.Logger.Info("Reconcile", zap.String("key", key))

	// Convert the namespace/name string into a distinct namespace and name.
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		logging.FromContext(ctx).Error("invalid resource key")
		return nil
	}

	// Get the KafkaChannel resource with this namespace/name.
	original, err := r.kafkachannelLister.KafkaChannels(namespace).Get(name)
	if err != nil {
		if apierrs.IsNotFound(err) {
			r.Logger.Warn("KafkaChannel No Longer Exists", zap.String("namespace", namespace), zap.String("name", name))
			return nil
		}
		r.Logger.Error("Error Retrieving KafkaChannel", zap.Error(err), zap.String("namespace", namespace), zap.String("name", name))
		return err
	}

	// Only Reconcile KafkaChannel Associated With This Dispatcher
	if r.dispatcher.ChannelKey != key {
		return nil
	}

	if !original.Status.IsReady() {
		return fmt.Errorf("Channel is not ready. Cannot configure and update subscriber status")
	}

	// Don't modify the informers copy
	channel := original.DeepCopy()

	reconcileError := r.reconcile(ctx, channel)
	if reconcileError != nil {
		r.Logger.Error("Error Reconciling KafkaChannel", zap.Error(reconcileError))
		r.Recorder.Eventf(channel, corev1.EventTypeWarning, channelReconcileFailed, "KafkaChannel Reconciliation Failed: %v", reconcileError)
	} else {
		r.Logger.Debug("KafkaChannel Reconciled Successfully")
		r.Recorder.Event(channel, corev1.EventTypeNormal, channelReconciled, "KafkaChannel Reconciled")
	}

	if _, updateStatusErr := r.updateStatus(ctx, channel); updateStatusErr != nil {
		r.Logger.Error("Failed to update KafkaChannel status", zap.Error(updateStatusErr))
		r.Recorder.Eventf(channel, corev1.EventTypeWarning, channelUpdateStatusFailed, "Failed to update KafkaChannel's status: %v", updateStatusErr)
		return updateStatusErr
	}

	return nil
}

func (r Reconciler) reconcile(ctx context.Context, channel *kafkav1alpha1.KafkaChannel) error {
	if channel.Spec.Subscribers == nil {
		return nil
	}

	subscriptions := make([]dispatcher.Subscription, 0)
	for _, subscriber := range channel.Spec.Subscribers {
		groupId := fmt.Sprintf("kafka.%s", subscriber.UID)
		subscriptions = append(subscriptions, dispatcher.Subscription{URI: subscriber.SubscriberURI, GroupId: groupId})
		r.Logger.Debug("Adding Subscriber, Consumer Group", zap.String("groupId", groupId), zap.String("URI", subscriber.SubscriberURI.String()))
	}

	failedSubscriptions := r.dispatcher.UpdateSubscriptions(subscriptions)

	channel.Status.SubscribableStatus = r.createSubscribableStatus(channel.Spec.SubscribableSpec, failedSubscriptions)
	if len(failedSubscriptions) > 0 {
		r.Logger.Error("Some kafka subscriptions failed to subscribe")
		return fmt.Errorf("Some kafka subscriptions failed to subscribe")
	}
	return nil
}

// Create The SubscribableStatus Block Based On The Updated Subscriptions
func (r *Reconciler) createSubscribableStatus(subscribable eventingduck.SubscribableSpec, failedSubscriptions map[dispatcher.Subscription]error) eventingduck.SubscribableStatus {
	subscriberStatus := make([]eventingduck.SubscriberStatus, 0)
	for _, sub := range subscribable.Subscribers {
		status := eventingduck.SubscriberStatus{
			UID:                sub.UID,
			ObservedGeneration: sub.Generation,
			Ready:              corev1.ConditionTrue,
		}
		groupId := fmt.Sprintf("kafka.%s", sub.UID)
		subscription := dispatcher.Subscription{URI: sub.SubscriberURI, GroupId: groupId}
		if err, ok := failedSubscriptions[subscription]; ok {
			status.Ready = corev1.ConditionFalse
			status.Message = err.Error()
		}
		subscriberStatus = append(subscriberStatus, status)
	}
	return eventingduck.SubscribableStatus{
		Subscribers: subscriberStatus,
	}
}

func (r *Reconciler) updateStatus(ctx context.Context, desired *kafkav1alpha1.KafkaChannel) (*kafkav1alpha1.KafkaChannel, error) {
	kc, err := r.kafkachannelLister.KafkaChannels(desired.Namespace).Get(desired.Name)
	if err != nil {
		return nil, err
	}

	if reflect.DeepEqual(kc.Status, desired.Status) {
		return kc, nil
	}

	// Don't modify the informers copy.
	existing := kc.DeepCopy()
	existing.Status = desired.Status
	new, err := r.KafkaClientSet.KnativekafkaV1alpha1().KafkaChannels(desired.Namespace).UpdateStatus(existing)
	return new, err
}
