package kubeletconfig

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/clarketm/json"
	ign3types "github.com/coreos/ignition/v2/config/v3_2/types"
	"github.com/golang/glog"
	"github.com/imdario/mergo"
	"github.com/vincent-petithory/dataurl"
	corev1 "k8s.io/api/core/v1"
	macherrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/jsonmergepatch"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	coreclientsetv1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	"k8s.io/client-go/util/workqueue"
	kubeletconfigv1beta1 "k8s.io/kubelet/config/v1beta1"

	oseinformersv1 "github.com/openshift/client-go/config/informers/externalversions/config/v1"
	oselistersv1 "github.com/openshift/client-go/config/listers/config/v1"
	mcfgv1 "github.com/openshift/machine-config-operator/pkg/apis/machineconfiguration.openshift.io/v1"
	ctrlcommon "github.com/openshift/machine-config-operator/pkg/controller/common"
	mtmpl "github.com/openshift/machine-config-operator/pkg/controller/template"
	mcfgclientset "github.com/openshift/machine-config-operator/pkg/generated/clientset/versioned"
	"github.com/openshift/machine-config-operator/pkg/generated/clientset/versioned/scheme"
	mcfginformersv1 "github.com/openshift/machine-config-operator/pkg/generated/informers/externalversions/machineconfiguration.openshift.io/v1"
	mcfglistersv1 "github.com/openshift/machine-config-operator/pkg/generated/listers/machineconfiguration.openshift.io/v1"
	"github.com/openshift/machine-config-operator/pkg/version"
)

const (
	// maxRetries is the number of times a machineconfig pool will be retried before it is dropped out of the queue.
	// With the current rate-limiter in use (5ms*2^(maxRetries-1)) the following numbers represent the times
	// a machineconfig pool is going to be requeued:
	//
	// 5ms, 10ms, 20ms, 40ms, 80ms, 160ms, 320ms, 640ms, 1.3s, 2.6s, 5.1s, 10.2s, 20.4s, 41s, 82s
	maxRetries = 15
)

var (
	// controllerKind contains the schema.GroupVersionKind for this controller type.
	controllerKind = mcfgv1.SchemeGroupVersion.WithKind("KubeletConfig")
)

var updateBackoff = wait.Backoff{
	Steps:    5,
	Duration: 100 * time.Millisecond,
	Jitter:   1.0,
}

var errCouldNotFindMCPSet = errors.New("could not find any MachineConfigPool set for KubeletConfig")

// Controller defines the kubelet config controller.
type Controller struct {
	templatesDir string

	client        mcfgclientset.Interface
	eventRecorder record.EventRecorder

	syncHandler          func(mcp string) error
	enqueueKubeletConfig func(*mcfgv1.KubeletConfig)

	ccLister       mcfglistersv1.ControllerConfigLister
	ccListerSynced cache.InformerSynced

	mckLister       mcfglistersv1.KubeletConfigLister
	mckListerSynced cache.InformerSynced

	mcpLister       mcfglistersv1.MachineConfigPoolLister
	mcpListerSynced cache.InformerSynced

	featLister       oselistersv1.FeatureGateLister
	featListerSynced cache.InformerSynced

	queue        workqueue.RateLimitingInterface
	featureQueue workqueue.RateLimitingInterface
}

// New returns a new kubelet config controller
func New(
	templatesDir string,
	mcpInformer mcfginformersv1.MachineConfigPoolInformer,
	ccInformer mcfginformersv1.ControllerConfigInformer,
	mkuInformer mcfginformersv1.KubeletConfigInformer,
	featInformer oseinformersv1.FeatureGateInformer,
	kubeClient clientset.Interface,
	mcfgClient mcfgclientset.Interface,
) *Controller {
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(glog.Infof)
	eventBroadcaster.StartRecordingToSink(&coreclientsetv1.EventSinkImpl{Interface: kubeClient.CoreV1().Events("")})

	ctrl := &Controller{
		templatesDir:  templatesDir,
		client:        mcfgClient,
		eventRecorder: eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: "machineconfigcontroller-kubeletconfigcontroller"}),
		queue:         workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "machineconfigcontroller-kubeletconfigcontroller"),
		featureQueue:  workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "machineconfigcontroller-featurecontroller"),
	}

	mkuInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    ctrl.addKubeletConfig,
		UpdateFunc: ctrl.updateKubeletConfig,
		DeleteFunc: ctrl.deleteKubeletConfig,
	})

	featInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    ctrl.addFeature,
		UpdateFunc: ctrl.updateFeature,
		DeleteFunc: ctrl.deleteFeature,
	})

	ctrl.syncHandler = ctrl.syncKubeletConfig
	ctrl.enqueueKubeletConfig = ctrl.enqueue

	ctrl.mcpLister = mcpInformer.Lister()
	ctrl.mcpListerSynced = mcpInformer.Informer().HasSynced

	ctrl.ccLister = ccInformer.Lister()
	ctrl.ccListerSynced = ccInformer.Informer().HasSynced

	ctrl.mckLister = mkuInformer.Lister()
	ctrl.mckListerSynced = mkuInformer.Informer().HasSynced

	ctrl.featLister = featInformer.Lister()
	ctrl.featListerSynced = featInformer.Informer().HasSynced

	return ctrl
}

// Run executes the kubelet config controller.
func (ctrl *Controller) Run(workers int, stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	defer ctrl.queue.ShutDown()
	defer ctrl.featureQueue.ShutDown()

	if !cache.WaitForCacheSync(stopCh, ctrl.mcpListerSynced, ctrl.mckListerSynced, ctrl.ccListerSynced, ctrl.featListerSynced) {
		return
	}

	glog.Info("Starting MachineConfigController-KubeletConfigController")
	defer glog.Info("Shutting down MachineConfigController-KubeletConfigController")

	for i := 0; i < workers; i++ {
		go wait.Until(ctrl.worker, time.Second, stopCh)
	}

	for i := 0; i < workers; i++ {
		go wait.Until(ctrl.featureWorker, time.Second, stopCh)
	}

	<-stopCh
}

func kubeletConfigTriggerObjectChange(old, new *mcfgv1.KubeletConfig) bool {
	if old.DeletionTimestamp != new.DeletionTimestamp {
		return true
	}
	if !reflect.DeepEqual(old.Spec, new.Spec) {
		return true
	}
	return false
}

func (ctrl *Controller) updateKubeletConfig(old, cur interface{}) {
	oldConfig := old.(*mcfgv1.KubeletConfig)
	newConfig := cur.(*mcfgv1.KubeletConfig)

	if kubeletConfigTriggerObjectChange(oldConfig, newConfig) {
		glog.V(4).Infof("Update KubeletConfig %s", oldConfig.Name)
		ctrl.enqueueKubeletConfig(newConfig)
	}
}

func (ctrl *Controller) addKubeletConfig(obj interface{}) {
	cfg := obj.(*mcfgv1.KubeletConfig)
	glog.V(4).Infof("Adding KubeletConfig %s", cfg.Name)
	ctrl.enqueueKubeletConfig(cfg)
}

func (ctrl *Controller) deleteKubeletConfig(obj interface{}) {
	cfg, ok := obj.(*mcfgv1.KubeletConfig)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("couldn't get object from tombstone %#v", obj))
			return
		}
		cfg, ok = tombstone.Obj.(*mcfgv1.KubeletConfig)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("Tombstone contained object that is not a KubeletConfig %#v", obj))
			return
		}
	}
	if err := ctrl.cascadeDelete(cfg); err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't delete object %#v: %v", cfg, err))
	} else {
		glog.V(4).Infof("Deleted KubeletConfig %s and restored default config", cfg.Name)
	}
}

func (ctrl *Controller) cascadeDelete(cfg *mcfgv1.KubeletConfig) error {
	if len(cfg.GetFinalizers()) == 0 {
		return nil
	}
	finalizerName := cfg.GetFinalizers()[0]
	mcs, err := ctrl.client.MachineconfigurationV1().MachineConfigs().List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return err
	}
	for _, mc := range mcs.Items {
		if string(mc.ObjectMeta.GetUID()) == finalizerName || mc.GetName() == finalizerName {
			err := ctrl.client.MachineconfigurationV1().MachineConfigs().Delete(context.TODO(), mc.GetName(), metav1.DeleteOptions{})
			if err != nil && !macherrors.IsNotFound(err) {
				return err
			}
			break
		}
	}
	if err := ctrl.popFinalizerFromKubeletConfig(cfg); err != nil {
		return err
	}
	return nil
}

func (ctrl *Controller) enqueue(cfg *mcfgv1.KubeletConfig) {
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(cfg)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %#v: %v", cfg, err))
		return
	}
	ctrl.queue.Add(key)
}

func (ctrl *Controller) enqueueRateLimited(cfg *mcfgv1.KubeletConfig) {
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(cfg)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %#v: %v", cfg, err))
		return
	}
	ctrl.queue.AddRateLimited(key)
}

// worker runs a worker thread that just dequeues items, processes them, and marks them done.
// It enforces that the syncHandler is never invoked concurrently with the same key.
func (ctrl *Controller) worker() {
	for ctrl.processNextWorkItem() {
	}
}

func (ctrl *Controller) processNextWorkItem() bool {
	key, quit := ctrl.queue.Get()
	if quit {
		return false
	}
	defer ctrl.queue.Done(key)

	err := ctrl.syncHandler(key.(string))
	ctrl.handleErr(err, key)

	return true
}

func (ctrl *Controller) handleErr(err error, key interface{}) {
	if err == nil {
		ctrl.queue.Forget(key)
		return
	}

	if _, ok := err.(*forgetError); ok {
		ctrl.queue.Forget(key)
		return
	}

	if ctrl.queue.NumRequeues(key) < maxRetries {
		glog.V(2).Infof("Error syncing kubeletconfig %v: %v", key, err)
		ctrl.queue.AddRateLimited(key)
		return
	}

	utilruntime.HandleError(err)
	glog.V(2).Infof("Dropping kubeletconfig %q out of the queue: %v", key, err)
	ctrl.queue.Forget(key)
	ctrl.queue.AddAfter(key, 1*time.Minute)
}

func (ctrl *Controller) handleFeatureErr(err error, key interface{}) {
	if err == nil {
		ctrl.featureQueue.Forget(key)
		return
	}

	if ctrl.featureQueue.NumRequeues(key) < maxRetries {
		glog.V(2).Infof("Error syncing kubeletconfig %v: %v", key, err)
		ctrl.featureQueue.AddRateLimited(key)
		return
	}

	utilruntime.HandleError(err)
	glog.V(2).Infof("Dropping featureconfig %q out of the queue: %v", key, err)
	ctrl.featureQueue.Forget(key)
	ctrl.featureQueue.AddAfter(key, 1*time.Minute)
}

func (ctrl *Controller) generateOriginalKubeletConfig(role string) (*ign3types.File, error) {
	cc, err := ctrl.ccLister.Get(ctrlcommon.ControllerConfigName)
	if err != nil {
		return nil, fmt.Errorf("could not get ControllerConfig %v", err)
	}
	// Render the default templates
	rc := &mtmpl.RenderConfig{ControllerConfigSpec: &cc.Spec}
	generatedConfigs, err := mtmpl.GenerateMachineConfigsForRole(rc, role, ctrl.templatesDir)
	if err != nil {
		return nil, fmt.Errorf("GenerateMachineConfigsforRole failed with error %s", err)
	}
	// Find generated kubelet.config
	for _, gmc := range generatedConfigs {
		gmcKubeletConfig, err := findKubeletConfig(gmc)
		if err != nil {
			continue
		}
		return gmcKubeletConfig, nil
	}
	return nil, fmt.Errorf("could not generate old kubelet config")
}

func (ctrl *Controller) syncStatusOnly(cfg *mcfgv1.KubeletConfig, err error, args ...interface{}) error {
	statusUpdateError := retry.RetryOnConflict(updateBackoff, func() error {
		newcfg, getErr := ctrl.mckLister.Get(cfg.Name)
		if getErr != nil {
			return getErr
		}
		// To avoid a long list of same statuses, only append a status if it is the first status
		// or if the status message is different from the message of the last status recorded
		// If the last status message is the same as the new one, then update the last status to
		// reflect the latest time stamp from the new status message.
		newStatusCondition := wrapErrorWithCondition(err, args...)
		if len(newcfg.Status.Conditions) == 0 || newStatusCondition.Message != newcfg.Status.Conditions[len(newcfg.Status.Conditions)-1].Message {
			newcfg.Status.Conditions = append(newcfg.Status.Conditions, newStatusCondition)
		} else if newcfg.Status.Conditions[len(newcfg.Status.Conditions)-1].Message == newStatusCondition.Message {
			newcfg.Status.Conditions[len(newcfg.Status.Conditions)-1] = newStatusCondition
		}
		_, lerr := ctrl.client.MachineconfigurationV1().KubeletConfigs().UpdateStatus(context.TODO(), newcfg, metav1.UpdateOptions{})
		return lerr
	})
	if statusUpdateError != nil {
		glog.Warningf("error updating kubeletconfig status: %v", statusUpdateError)
	}
	return err
}

// addAnnotation adds the annotions for a kubeletconfig object with the given annotationKey and annotationVal
func (ctrl *Controller) addAnnotation(cfg *mcfgv1.KubeletConfig, annotationKey, annotationVal string) error {
	annotationUpdateErr := retry.RetryOnConflict(updateBackoff, func() error {
		newcfg, getErr := ctrl.mckLister.Get(cfg.Name)
		if getErr != nil {
			return getErr
		}
		newcfg.SetAnnotations(map[string]string{
			annotationKey: annotationVal,
		})
		_, updateErr := ctrl.client.MachineconfigurationV1().KubeletConfigs().Update(context.TODO(), newcfg, metav1.UpdateOptions{})
		return updateErr
	})
	if annotationUpdateErr != nil {
		glog.Warningf("error updating the kubelet config with annotation key %q and value %q: %v", annotationKey, annotationVal, annotationUpdateErr)
	}
	return annotationUpdateErr
}

// syncKubeletConfig will sync the kubeletconfig with the given key.
// This function is not meant to be invoked concurrently with the same key.
//nolint:gocyclo
func (ctrl *Controller) syncKubeletConfig(key string) error {
	startTime := time.Now()
	glog.V(4).Infof("Started syncing kubeletconfig %q (%v)", key, startTime)
	defer func() {
		glog.V(4).Infof("Finished syncing kubeletconfig %q (%v)", key, time.Since(startTime))
	}()

	_, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}

	// Fetch the KubeletConfig
	cfg, err := ctrl.mckLister.Get(name)
	if macherrors.IsNotFound(err) {
		glog.V(2).Infof("KubeletConfig %v has been deleted", key)
		return nil
	}
	if err != nil {
		return err
	}

	// Deep-copy otherwise we are mutating our cache.
	cfg = cfg.DeepCopy()

	// Check for Deleted KubeletConfig and optionally delete finalizers
	if cfg.DeletionTimestamp != nil {
		if len(cfg.GetFinalizers()) > 0 {
			return ctrl.cascadeDelete(cfg)
		}
		return nil
	}

	// If we have seen this generation then skip
	if cfg.Status.ObservedGeneration >= cfg.Generation {
		return nil
	}

	// Validate the KubeletConfig CR
	if err := validateUserKubeletConfig(cfg); err != nil {
		return ctrl.syncStatusOnly(cfg, newForgetError(err))
	}

	// Find all MachineConfigPools
	mcpPools, err := ctrl.getPoolsForKubeletConfig(cfg)
	if err != nil {
		return ctrl.syncStatusOnly(cfg, err)
	}

	if len(mcpPools) == 0 {
		err := fmt.Errorf("KubeletConfig %v does not match any MachineConfigPools", key)
		glog.V(2).Infof("%v", err)
		return ctrl.syncStatusOnly(cfg, err)
	}

	features, err := ctrl.featLister.Get(clusterFeatureInstanceName)
	if macherrors.IsNotFound(err) {
		features = createNewDefaultFeatureGate()
	} else if err != nil {
		glog.V(2).Infof("%v", err)
		err := fmt.Errorf("could not fetch FeatureGates: %v", err)
		return ctrl.syncStatusOnly(cfg, err)
	}
	featureGates, err := ctrl.generateFeatureMap(features)
	if err != nil {
		err := fmt.Errorf("could not generate FeatureMap: %v", err)
		glog.V(2).Infof("%v", err)
		return ctrl.syncStatusOnly(cfg, err)
	}

	for _, pool := range mcpPools {
		role := pool.Name
		// Get MachineConfig
		managedKey, err := getManagedKubeletConfigKey(pool, ctrl.client, cfg)
		if err != nil {
			return ctrl.syncStatusOnly(cfg, err, "could not get kubelet config key: %v", err)
		}
		mc, err := ctrl.client.MachineconfigurationV1().MachineConfigs().Get(context.TODO(), managedKey, metav1.GetOptions{})
		if err != nil && !macherrors.IsNotFound(err) {
			return ctrl.syncStatusOnly(cfg, err, "could not find MachineConfig: %v", managedKey)
		}
		isNotFound := macherrors.IsNotFound(err)

		var kubeletIgnition *ign3types.File
		var logLevelIgnition *ign3types.File

		if cfg.Spec.KubeletConfig != nil && cfg.Spec.KubeletConfig.Raw != nil {
			// Generate the original KubeletConfig
			originalKubeletIgn, err := ctrl.generateOriginalKubeletConfig(role)
			if err != nil {
				return ctrl.syncStatusOnly(cfg, err, "could not generate the original Kubelet config: %v", err)
			}
			if originalKubeletIgn.Contents.Source == nil {
				return ctrl.syncStatusOnly(cfg, err, "the original Kubelet source string is empty: %v", err)
			}
			dataURL, err := dataurl.DecodeString(*originalKubeletIgn.Contents.Source)
			if err != nil {
				return ctrl.syncStatusOnly(cfg, err, "could not decode the original Kubelet source string: %v", err)
			}
			originalKubeConfig, err := decodeKubeletConfig(dataURL.Data)
			if err != nil {
				return ctrl.syncStatusOnly(cfg, err, "could not deserialize the Kubelet source: %v", err)
			}
			specKubeletConfig, err := decodeKubeletConfig(cfg.Spec.KubeletConfig.Raw)
			if err != nil {
				return ctrl.syncStatusOnly(cfg, err, "could not deserialize the new Kubelet config: %v", err)
			}
			// Merge the Old and New
			err = mergo.Merge(originalKubeConfig, specKubeletConfig, mergo.WithOverride)
			if err != nil {
				return ctrl.syncStatusOnly(cfg, err, "could not merge original config and new config: %v", err)
			}
			// Merge in Feature Gates
			err = mergo.Merge(&originalKubeConfig.FeatureGates, featureGates, mergo.WithOverride)
			if err != nil {
				return ctrl.syncStatusOnly(cfg, err, "could not merge FeatureGates: %v", err)
			}
			// Encode the new config into raw JSON
			cfgJSON, err := EncodeKubeletConfig(originalKubeConfig, kubeletconfigv1beta1.SchemeGroupVersion)
			if err != nil {
				return ctrl.syncStatusOnly(cfg, err, "could not encode JSON: %v", err)
			}
			kubeletIgnition = createNewKubeletIgnition(cfgJSON)
		}

		if isNotFound {
			ignConfig := ctrlcommon.NewIgnConfig()
			mc, err = ctrlcommon.MachineConfigFromIgnConfig(role, managedKey, ignConfig)
			if err != nil {
				return ctrl.syncStatusOnly(cfg, err, "could not create MachineConfig from new Ignition config: %v", err)
			}
			mc.ObjectMeta.UID = uuid.NewUUID()
			_, ok := cfg.GetAnnotations()[ctrlcommon.MCNameSuffixAnnotationKey]
			arr := strings.Split(managedKey, "-")
			// If the MC name suffix annotation does not exist and the managed key value returned has a suffix, then add the MC name
			// suffix annotation and suffix value to the kubelet config object
			if len(arr) > 4 && !ok {
				_, err := strconv.Atoi(arr[len(arr)-1])
				if err == nil {
					if err := ctrl.addAnnotation(cfg, ctrlcommon.MCNameSuffixAnnotationKey, arr[len(arr)-1]); err != nil {
						return ctrl.syncStatusOnly(cfg, err, "could not update annotation for kubeletConfig")
					}
				}
			}
		}

		if cfg.Spec.LogLevel != nil {
			logLevelIgnition = createNewKubeletLogLevelIgnition(*cfg.Spec.LogLevel)
		}

		tempIgnConfig := ctrlcommon.NewIgnConfig()
		if logLevelIgnition != nil {
			tempIgnConfig.Storage.Files = append(tempIgnConfig.Storage.Files, *logLevelIgnition)
		}
		if kubeletIgnition != nil {
			tempIgnConfig.Storage.Files = append(tempIgnConfig.Storage.Files, *kubeletIgnition)
		}

		rawIgn, err := json.Marshal(tempIgnConfig)
		if err != nil {
			return ctrl.syncStatusOnly(cfg, err, "could not marshal kubelet config Ignition: %v", err)
		}
		mc.Spec.Config.Raw = rawIgn

		mc.SetAnnotations(map[string]string{
			ctrlcommon.GeneratedByControllerVersionAnnotationKey: version.Hash,
		})
		oref := metav1.NewControllerRef(cfg, controllerKind)
		mc.SetOwnerReferences([]metav1.OwnerReference{*oref})

		// Create or Update, on conflict retry
		if err := retry.RetryOnConflict(updateBackoff, func() error {
			var err error
			if isNotFound {
				_, err = ctrl.client.MachineconfigurationV1().MachineConfigs().Create(context.TODO(), mc, metav1.CreateOptions{})
			} else {
				_, err = ctrl.client.MachineconfigurationV1().MachineConfigs().Update(context.TODO(), mc, metav1.UpdateOptions{})
			}
			return err
		}); err != nil {
			return ctrl.syncStatusOnly(cfg, err, "could not Create/Update MachineConfig: %v", err)
		}
		// Add Finalizers to the KubletConfig
		if err := ctrl.addFinalizerToKubeletConfig(cfg, mc); err != nil {
			return ctrl.syncStatusOnly(cfg, err, "could not add finalizers to KubeletConfig: %v", err)
		}
		glog.Infof("Applied KubeletConfig %v on MachineConfigPool %v", key, pool.Name)
	}
	if err := ctrl.cleanUpDuplicatedMC(); err != nil {
		return err
	}

	return ctrl.syncStatusOnly(cfg, nil)
}

// cleanUpDuplicatedMC removes the MC of uncorrected version if format of its name contains 'generated-xxx'.
// BZ 1955517: upgrade when there are more than one configs, these generated MC will be duplicated
// by upgraded MC with number suffixed name (func getManagedKubeletConfigKey()) and fails the upgrade.
func (ctrl *Controller) cleanUpDuplicatedMC() error {
	generatedKubeletCfg := "generated-kubelet"
	// Get all machine configs
	mcList, err := ctrl.client.MachineconfigurationV1().MachineConfigs().List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("error listing kubelet machine configs: %v", err)
	}
	for _, mc := range mcList.Items {
		if !strings.Contains(mc.Name, generatedKubeletCfg) {
			continue
		}
		// delete the mc if its degraded
		if mc.Annotations[ctrlcommon.GeneratedByControllerVersionAnnotationKey] != version.Hash {
			if err := ctrl.client.MachineconfigurationV1().MachineConfigs().Delete(context.TODO(), mc.Name, metav1.DeleteOptions{}); err != nil {
				return fmt.Errorf("error deleting degraded kubelet machine config %s: %v", mc.Name, err)
			}
		}
	}
	return nil
}

func (ctrl *Controller) popFinalizerFromKubeletConfig(kc *mcfgv1.KubeletConfig) error {
	return retry.RetryOnConflict(updateBackoff, func() error {
		newcfg, err := ctrl.mckLister.Get(kc.Name)
		if macherrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}

		curJSON, err := json.Marshal(newcfg)
		if err != nil {
			return err
		}

		kcTmp := newcfg.DeepCopy()
		kcTmp.Finalizers = append(kc.Finalizers[:0], kc.Finalizers[1:]...)

		modJSON, err := json.Marshal(kcTmp)
		if err != nil {
			return err
		}

		patch, err := jsonmergepatch.CreateThreeWayJSONMergePatch(curJSON, modJSON, curJSON)
		if err != nil {
			return err
		}
		return ctrl.patchKubeletConfigs(newcfg.Name, patch)
	})
}

func (ctrl *Controller) patchKubeletConfigs(name string, patch []byte) error {
	_, err := ctrl.client.MachineconfigurationV1().KubeletConfigs().Patch(context.TODO(), name, types.MergePatchType, patch, metav1.PatchOptions{})
	return err
}

func (ctrl *Controller) addFinalizerToKubeletConfig(kc *mcfgv1.KubeletConfig, mc *mcfgv1.MachineConfig) error {
	return retry.RetryOnConflict(updateBackoff, func() error {
		newcfg, err := ctrl.mckLister.Get(kc.Name)
		if macherrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}

		curJSON, err := json.Marshal(newcfg)
		if err != nil {
			return err
		}

		kcTmp := newcfg.DeepCopy()
		// We want to use the mc name as the finalizer instead of the uid because
		// every time a resync happens, a new uid is generated. This is why the list
		// of finalizers had multiple entries. So check if the list of finalizers consists
		// of uids, if it does then clear the list of finalizers and we will add the mc
		// name to it, ensuring we don't have duplicate or multiple finalizers.
		for _, finalizerName := range newcfg.Finalizers {
			if !strings.Contains(finalizerName, "kubelet") {
				kcTmp.ObjectMeta.SetFinalizers([]string{})
			}
		}
		// Only append the mc name if it is not already in the list of finalizers.
		// When we update an existing kubeletconfig, the generation number increases causing
		// a resync to happen. When this happens, the mc name is the same, so we don't
		// want to add duplicate entries to the list of finalizers.
		if !ctrlcommon.InSlice(mc.Name, kcTmp.ObjectMeta.Finalizers) {
			kcTmp.ObjectMeta.Finalizers = append(kcTmp.ObjectMeta.Finalizers, mc.Name)
		}

		modJSON, err := json.Marshal(kcTmp)
		if err != nil {
			return err
		}
		patch, err := jsonmergepatch.CreateThreeWayJSONMergePatch(curJSON, modJSON, curJSON)
		if err != nil {
			return err
		}
		return ctrl.patchKubeletConfigs(newcfg.Name, patch)
	})
}

func (ctrl *Controller) getPoolsForKubeletConfig(config *mcfgv1.KubeletConfig) ([]*mcfgv1.MachineConfigPool, error) {
	pList, err := ctrl.mcpLister.List(labels.Everything())
	if err != nil {
		return nil, err
	}

	selector, err := metav1.LabelSelectorAsSelector(config.Spec.MachineConfigPoolSelector)
	if err != nil {
		return nil, fmt.Errorf("invalid label selector: %v", err)
	}

	var pools []*mcfgv1.MachineConfigPool
	for _, p := range pList {
		// If a pool with a nil or empty selector creeps in, it should match nothing, not everything.
		if selector.Empty() || !selector.Matches(labels.Set(p.Labels)) {
			continue
		}
		pools = append(pools, p)
	}

	if len(pools) == 0 {
		return nil, errCouldNotFindMCPSet
	}

	return pools, nil
}
