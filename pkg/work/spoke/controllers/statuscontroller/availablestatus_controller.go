package statuscontroller

import (
	"context"
	"fmt"
	"time"

	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/klog/v2"

	workv1client "open-cluster-management.io/api/client/work/clientset/versioned/typed/work/v1"
	workinformer "open-cluster-management.io/api/client/work/informers/externalversions/work/v1"
	worklister "open-cluster-management.io/api/client/work/listers/work/v1"
	workapiv1 "open-cluster-management.io/api/work/v1"
	"open-cluster-management.io/sdk-go/pkg/patcher"

	commonhelper "open-cluster-management.io/ocm/pkg/common/helpers"
	"open-cluster-management.io/ocm/pkg/common/queue"
	"open-cluster-management.io/ocm/pkg/work/helper"
	"open-cluster-management.io/ocm/pkg/work/spoke/conditions"
	"open-cluster-management.io/ocm/pkg/work/spoke/statusfeedback"
)

const statusFeedbackConditionType = "StatusFeedbackSynced"

// AvailableStatusController is to update the available status conditions of both manifests and manifestworks.
// It is also used to get the status value based on status feedback configuration and get condition values
// based on condition rules in manifestwork. The functions are logically disinct, however, they are put in the same
// control loop to reduce live get call to spoke apiserver and status update call to hub apiserver.
type AvailableStatusController struct {
	patcher            patcher.Patcher[*workapiv1.ManifestWork, workapiv1.ManifestWorkSpec, workapiv1.ManifestWorkStatus]
	manifestWorkLister worklister.ManifestWorkNamespaceLister
	spokeDynamicClient dynamic.Interface
	statusReader       *statusfeedback.StatusReader
	conditionReader    *conditions.ConditionReader
	syncInterval       time.Duration
}

// NewAvailableStatusController returns a AvailableStatusController
func NewAvailableStatusController(
	recorder events.Recorder,
	spokeDynamicClient dynamic.Interface,
	manifestWorkClient workv1client.ManifestWorkInterface,
	manifestWorkInformer workinformer.ManifestWorkInformer,
	manifestWorkLister worklister.ManifestWorkNamespaceLister,
	maxJSONRawLength int32,
	syncInterval time.Duration,
) (factory.Controller, error) {
	conditionReader, err := conditions.NewConditionReader()
	if err != nil {
		return nil, err
	}

	controller := &AvailableStatusController{
		patcher: patcher.NewPatcher[
			*workapiv1.ManifestWork, workapiv1.ManifestWorkSpec, workapiv1.ManifestWorkStatus](
			manifestWorkClient),
		manifestWorkLister: manifestWorkLister,
		spokeDynamicClient: spokeDynamicClient,
		syncInterval:       syncInterval,
		statusReader:       statusfeedback.NewStatusReader().WithMaxJsonRawLength(maxJSONRawLength),
		conditionReader:    conditionReader,
	}

	return factory.New().
		WithInformersQueueKeysFunc(queue.QueueKeyByMetaName, manifestWorkInformer.Informer()).
		WithSync(controller.sync).ToController("AvailableStatusController", recorder), nil
}

func (c *AvailableStatusController) sync(ctx context.Context, controllerContext factory.SyncContext) error {
	manifestWorkName := controllerContext.QueueKey()
	// sync a particular manifestwork
	manifestWork, err := c.manifestWorkLister.Get(manifestWorkName)
	if errors.IsNotFound(err) {
		// work not found, could have been deleted, do nothing.
		return nil
	}
	if err != nil {
		return fmt.Errorf("unable to fetch manifestwork %q: %w", manifestWorkName, err)
	}

	err = c.syncManifestWork(ctx, manifestWork)
	if err != nil {
		return fmt.Errorf("unable to sync manifestwork %q: %w", manifestWork.Name, err)
	}

	// requeue with a certain jitter
	controllerContext.Queue().AddAfter(manifestWorkName, wait.Jitter(c.syncInterval, 0.9))
	return nil
}

func (c *AvailableStatusController) syncManifestWork(ctx context.Context, originalManifestWork *workapiv1.ManifestWork) error {
	klog.V(5).Infof("Reconciling ManifestWork %q", originalManifestWork.Name)
	manifestWork := originalManifestWork.DeepCopy()

	// do nothing when finalizer is not added.
	if !commonhelper.HasFinalizer(manifestWork.Finalizers, workapiv1.ManifestWorkFinalizer) {
		return nil
	}

	// wait until work has the applied condition.
	if cond := meta.FindStatusCondition(manifestWork.Status.Conditions, workapiv1.WorkApplied); cond == nil {
		return nil
	}

	// handle status condition of manifests
	// TODO revist this controller since this might bring races when user change the manifests in spec.
	for index, manifest := range manifestWork.Status.ResourceStatus.Manifests {
		obj, availableStatusCondition, err := buildAvailableStatusCondition(manifest.ResourceMeta, c.spokeDynamicClient)
		manifestConditions := &manifestWork.Status.ResourceStatus.Manifests[index].Conditions
		meta.SetStatusCondition(manifestConditions, availableStatusCondition)
		if err != nil {
			// skip getting status values if resource is not available.
			continue
		}

		option := helper.FindManifestConfiguration(manifest.ResourceMeta, manifestWork.Spec.ManifestConfigs)

		// Read status of the resource according to feedback rules.
		values, statusFeedbackCondition := c.getFeedbackValues(obj, option)
		meta.SetStatusCondition(manifestConditions, statusFeedbackCondition)
		manifestWork.Status.ResourceStatus.Manifests[index].StatusFeedbacks.Values = values

		// Read conditions according to condition rules
		conditions := c.evaluateConditions(ctx, obj, option)
		for _, condition := range conditions {
			meta.SetStatusCondition(manifestConditions, condition)
		}
	}

	// aggregate ManifestConditions and update work status condition
	workAvailableStatusCondition := aggregateManifestConditions(manifestWork.Generation, manifestWork.Status.ResourceStatus.Manifests)
	meta.SetStatusCondition(&manifestWork.Status.Conditions, workAvailableStatusCondition)

	// no work if the status of manifestwork does not change
	if equality.Semantic.DeepEqual(originalManifestWork.Status.ResourceStatus, manifestWork.Status.ResourceStatus) &&
		equality.Semantic.DeepEqual(originalManifestWork.Status.Conditions, manifestWork.Status.Conditions) {
		return nil
	}

	// update status of manifestwork. if this conflicts, try again later
	_, err := c.patcher.PatchStatus(ctx, manifestWork, manifestWork.Status, originalManifestWork.Status)
	return err
}

// aggregateManifestConditions aggregates status conditions of manifests and returns a status
// condition for manifestwork
func aggregateManifestConditions(generation int64, manifests []workapiv1.ManifestCondition) metav1.Condition {
	available, unavailable, unknown := 0, 0, 0
	for _, manifest := range manifests {
		for _, condition := range manifest.Conditions {
			if condition.Type != workapiv1.ManifestAvailable {
				continue
			}

			switch condition.Status {
			case metav1.ConditionTrue:
				available += 1
			case metav1.ConditionFalse:
				unavailable += 1
			case metav1.ConditionUnknown:
				unknown += 1
			}
		}
	}

	switch {
	case unavailable > 0:
		return metav1.Condition{
			Type:               workapiv1.WorkAvailable,
			Status:             metav1.ConditionFalse,
			Reason:             "ResourcesNotAvailable",
			ObservedGeneration: generation,
			Message:            fmt.Sprintf("%d of %d resources are not available", unavailable, len(manifests)),
		}
	case unknown > 0:
		return metav1.Condition{
			Type:               workapiv1.WorkAvailable,
			Status:             metav1.ConditionUnknown,
			Reason:             "ResourcesStatusUnknown",
			ObservedGeneration: generation,
			Message:            fmt.Sprintf("%d of %d resources have unknown status", unknown, len(manifests)),
		}
	case available == 0:
		return metav1.Condition{
			Type:               workapiv1.WorkAvailable,
			Status:             metav1.ConditionUnknown,
			Reason:             "ResourcesStatusUnknown",
			ObservedGeneration: generation,
			Message:            "cannot get any available resource ",
		}
	default:
		return metav1.Condition{
			Type:               workapiv1.WorkAvailable,
			Status:             metav1.ConditionTrue,
			Reason:             "ResourcesAvailable",
			ObservedGeneration: generation,
			Message:            "All resources are available",
		}
	}
}

func (c *AvailableStatusController) getFeedbackValues(
	obj *unstructured.Unstructured,
	option *workapiv1.ManifestConfigOption) ([]workapiv1.FeedbackValue, metav1.Condition) {
	var errs []error
	var values []workapiv1.FeedbackValue

	if option == nil || len(option.FeedbackRules) == 0 {
		return values, metav1.Condition{
			Type:   statusFeedbackConditionType,
			Reason: "NoStatusFeedbackSynced",
			Status: metav1.ConditionTrue,
		}
	}

	for _, rule := range option.FeedbackRules {
		valuesByRule, err := c.statusReader.GetValuesByRule(obj, rule)
		if err != nil {
			errs = append(errs, err)
		}
		if len(valuesByRule) > 0 {
			values = append(values, valuesByRule...)
		}
	}

	err := utilerrors.NewAggregate(errs)

	if err != nil {
		return values, metav1.Condition{
			Type:    statusFeedbackConditionType,
			Reason:  "StatusFeedbackSyncFailed",
			Status:  metav1.ConditionFalse,
			Message: fmt.Sprintf("Sync status feedback failed with error %v", err),
		}
	}

	return values, metav1.Condition{
		Type:   statusFeedbackConditionType,
		Reason: "StatusFeedbackSynced",
		Status: metav1.ConditionTrue,
	}
}

func (c *AvailableStatusController) evaluateConditions(
	ctx context.Context, obj *unstructured.Unstructured, option *workapiv1.ManifestConfigOption,
) []metav1.Condition {
	if option == nil || len(option.ConditionRules) == 0 {
		return nil
	}

	return c.conditionReader.EvaluateConditions(ctx, obj, option.ConditionRules)
}

// buildAvailableStatusCondition returns a StatusCondition with type Available for a given manifest resource
func buildAvailableStatusCondition(resourceMeta workapiv1.ManifestResourceMeta,
	dynamicClient dynamic.Interface) (*unstructured.Unstructured, metav1.Condition, error) {
	conditionType := workapiv1.ManifestAvailable

	if len(resourceMeta.Resource) == 0 || len(resourceMeta.Version) == 0 || len(resourceMeta.Name) == 0 {
		return nil, metav1.Condition{
			Type:    conditionType,
			Status:  metav1.ConditionUnknown,
			Reason:  "IncompletedResourceMeta",
			Message: "Resource meta is incompleted",
		}, fmt.Errorf("incomplete resource meta")
	}

	gvr := schema.GroupVersionResource{
		Group:    resourceMeta.Group,
		Version:  resourceMeta.Version,
		Resource: resourceMeta.Resource,
	}

	obj, err := dynamicClient.Resource(gvr).Namespace(resourceMeta.Namespace).Get(context.TODO(), resourceMeta.Name, metav1.GetOptions{})

	switch {
	case errors.IsNotFound(err):
		return nil, metav1.Condition{
			Type:    conditionType,
			Status:  metav1.ConditionFalse,
			Reason:  "ResourceNotAvailable",
			Message: "Resource is not available",
		}, err
	case err != nil:
		return nil, metav1.Condition{
			Type:    conditionType,
			Status:  metav1.ConditionUnknown,
			Reason:  "FetchingResourceFailed",
			Message: fmt.Sprintf("Failed to fetch resource: %v", err),
		}, err
	}

	return obj, metav1.Condition{
		Type:    conditionType,
		Status:  metav1.ConditionTrue,
		Reason:  "ResourceAvailable",
		Message: "Resource is available",
	}, nil
}
