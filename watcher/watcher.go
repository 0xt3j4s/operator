package watcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/armosec/armoapi-go/apis"
	pkgwlid "github.com/armosec/utils-k8s-go/wlid"
	"github.com/kubescape/go-logger"
	"github.com/kubescape/go-logger/helpers"
	"github.com/kubescape/k8s-interface/instanceidhandler"
	instanceidhandlerv1 "github.com/kubescape/k8s-interface/instanceidhandler/v1"
	"github.com/kubescape/k8s-interface/k8sinterface"
	"github.com/kubescape/k8s-interface/workloadinterface"
	"github.com/kubescape/operator/utils"
	spdxv1beta1 "github.com/kubescape/storage/pkg/apis/softwarecomposition/v1beta1"
	kssc "github.com/kubescape/storage/pkg/generated/clientset/versioned"
	"golang.org/x/exp/slices"
	core1 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
)

const (
	retryInterval = 3 * time.Second

	instanceIDAnnotationKey = "instanceID"
)

var (
	ErrUnsupportedObject = errors.New("unsupported object type")
	ErrUnknownImageHash  = errors.New("unknown image hash")
)

type WlidsToContainerToImageIDMap map[string]map[string]string

type WatchHandler struct {
	k8sAPI                            *k8sinterface.KubernetesApi
	storageClient                     kssc.Interface
	iwMap                             *imageHashWLIDMap
	hashedInstanceIDs                 []string
	instanceIDsMutex                  *sync.RWMutex
	wlidsToContainerToImageIDMap      WlidsToContainerToImageIDMap // <wlid> : <containerName> : imageID
	wlidsToContainerToImageIDMapMutex *sync.RWMutex
	currentPodListResourceVersion     string // current PodList version, used by watcher (https://kubernetes.io/docs/reference/using-api/api-concepts/#efficient-detection-of-changes)
}

// remove unused imageIDs and instanceIDs from storage. Update internal maps
func (wh *WatchHandler) cleanUp(ctx context.Context) {
	// list Pods, extract their imageIDs and instanceIDs
	podsList, err := wh.k8sAPI.ListPods("", map[string]string{})
	if err != nil {
		logger.L().Ctx(ctx).Error("could not complete cleanUp routine: error to ListPods", helpers.Error(err))
		return
	}

	// reset maps - clean them and build them again
	wh.cleanUpIDs()
	wh.buildIDs(ctx, podsList)
}

// NewWatchHandler creates a new WatchHandler, initializes the maps and returns it
func NewWatchHandler(ctx context.Context, k8sAPI *k8sinterface.KubernetesApi, storageClient kssc.Interface, imageIDsToWLIDsMap map[string][]string, instanceIDs []string) (*WatchHandler, error) {

	wh := &WatchHandler{
		storageClient:                     storageClient,
		k8sAPI:                            k8sAPI,
		iwMap:                             NewImageHashWLIDsMapFrom(imageIDsToWLIDsMap),
		wlidsToContainerToImageIDMap:      make(WlidsToContainerToImageIDMap),
		wlidsToContainerToImageIDMapMutex: &sync.RWMutex{},
		instanceIDsMutex:                  &sync.RWMutex{},
		hashedInstanceIDs:                 instanceIDs,
	}

	// list all Pods and extract their image IDs
	podsList, err := wh.k8sAPI.ListPods("", map[string]string{})
	if err != nil {
		return nil, err
	}

	wh.buildIDs(ctx, podsList)

	wh.currentPodListResourceVersion = podsList.GetResourceVersion()

	wh.startCleanUpAndTriggerScanRoutine(ctx)

	return wh, nil
}

// start routine which cleans up unused imageIDs and instanceIDs from storage, and  triggers relevancy scan
func (wh *WatchHandler) startCleanUpAndTriggerScanRoutine(ctx context.Context) {
	go func() {
		for {
			time.Sleep(utils.CleanUpRoutineInterval)
			wh.cleanUp(ctx)
			// must be called after cleanUp, since we can have two instanceIDs with same wlid
			// wh.triggerRelevancyScan(ctx)
		}
	}()
}

func (wh *WatchHandler) listInstanceIDs() []string {
	wh.instanceIDsMutex.RLock()
	defer wh.instanceIDsMutex.RUnlock()

	return wh.hashedInstanceIDs
}

// returns wlids map
func (wh *WatchHandler) GetWlidsToContainerToImageIDMap() WlidsToContainerToImageIDMap {
	wh.wlidsToContainerToImageIDMapMutex.RLock()
	defer wh.wlidsToContainerToImageIDMapMutex.RUnlock()

	return wh.wlidsToContainerToImageIDMap
}

func labelsToInstanceID(labels map[string]string) (string, error) {
	instanceID, ok := labels[instanceIDAnnotationKey]
	if !ok {
		return instanceID, ErrMissingInstanceIDAnnotation
	}
	return instanceID, nil
}

func (wh *WatchHandler) getVulnerabilityManifestWatcher() (watch.Interface, error) {
	return wh.storageClient.SpdxV1beta1().VulnerabilityManifests("").Watch(context.TODO(), v1.ListOptions{})
}

// VulnerabilityManifestWatch watches for Vulnerability Manifests and handles them accordingly
func (wh *WatchHandler) VulnerabilityManifestWatch(ctx context.Context, sessionObjChan *chan utils.SessionObj) {
	inputEvents := make(chan watch.Event)
	errorCh := make(chan error)
	vmEvents := make(<-chan watch.Event)

	// The watcher is considered unavailable by default
	watcherUnavailable := make(chan struct{})
	go func() {
		watcherUnavailable <- struct{}{}
	}()

	go wh.HandleVulnerabilityManifestEvents(inputEvents, errorCh)

	// notifyWatcherDown notifies the appropriate channel that the watcher
	// is down and backs off for the retry interval to not produce
	// unnecessary events
	notifyWatcherDown := func(watcherDownCh chan<- struct{}) {
		go func() { watcherDownCh <- struct{}{} }()
		time.Sleep(retryInterval)
	}

	var watcher watch.Interface
	var err error
	for {
		select {
		case event, ok := <-vmEvents:
			if ok {
				inputEvents <- event
			} else {
				notifyWatcherDown(watcherUnavailable)
			}
		case err, ok := <-errorCh:
			if ok {
				logger.L().Ctx(ctx).Error(fmt.Sprintf("error in SBOMWatch: %v", err.Error()))
			} else {
				notifyWatcherDown(watcherUnavailable)
			}
		case <-watcherUnavailable:
			if watcher != nil {
				watcher.Stop()
			}

			watcher, err = wh.getVulnerabilityManifestWatcher()
			if err != nil {
				notifyWatcherDown(watcherUnavailable)
			} else {
				vmEvents = watcher.ResultChan()
			}
		}
	}
}

func (wh *WatchHandler) HandleVulnerabilityManifestEvents(vmEvents <-chan watch.Event, errorCh chan<- error) {
	defer close(errorCh)

	for e := range vmEvents {
		if e.Type == watch.Deleted {
			continue
		}

		obj, ok := e.Object.(*spdxv1beta1.VulnerabilityManifest)
		if !ok {
			errorCh <- ErrUnsupportedObject
			continue
		}

		manifestName := obj.ObjectMeta.Name
		imageHash := manifestName
		withRelevancy := obj.Spec.Metadata.WithRelevancy

		var hasObject bool
		if withRelevancy {
			instanceIDs := wh.listInstanceIDs()
			hashedInstanceID := manifestName
			hasObject = slices.Contains(instanceIDs, hashedInstanceID)
		} else {
			_, hasObject = wh.iwMap.Load(imageHash)
		}

		if !hasObject {
			wh.storageClient.SpdxV1beta1().VulnerabilityManifests(obj.ObjectMeta.Namespace).Delete(context.TODO(), manifestName, v1.DeleteOptions{})
		}
	}
}

func (wh *WatchHandler) HandleSBOMFilteredEvents(sfEvents <-chan watch.Event, producedCommands chan<- *apis.Command, errorCh chan<- error) {
	defer close(errorCh)

	for e := range sfEvents {
		obj, ok := e.Object.(*spdxv1beta1.SBOMSPDXv2p3Filtered)
		if !ok {
			errorCh <- ErrUnsupportedObject
			continue
		}

		// Deleting an already deleted object makes no sense
		if e.Type == watch.Deleted {
			continue
		}

		hashedInstanceID := obj.ObjectMeta.Name

		if !slices.Contains(wh.hashedInstanceIDs, hashedInstanceID) {
			wh.storageClient.SpdxV1beta1().SBOMSPDXv2p3Filtereds(obj.ObjectMeta.Namespace).Delete(context.TODO(), obj.ObjectMeta.Name, v1.DeleteOptions{})
			continue
		}

		wlid, ok := obj.ObjectMeta.Annotations[instanceidhandlerv1.WlidAnnotationKey]
		if !ok {
			errorCh <- ErrMissingWLIDAnnotation
			continue
		}

		containerToImageIDs := wh.GetContainerToImageIDForWlid(wlid)
		cmd := getImageScanCommand(wlid, containerToImageIDs)
		producedCommands <- cmd
	}
}

// HandleSBOMEvents handles SBOM-related events
//
// Handling events is defined as deleting SBOMs that are not known to the Operator
func (wh *WatchHandler) HandleSBOMEvents(sbomEvents <-chan watch.Event, errorCh chan<- error) {
	defer close(errorCh)

	for event := range sbomEvents {
		obj, ok := event.Object.(*spdxv1beta1.SBOMSPDXv2p3)
		if !ok {
			errorCh <- ErrUnsupportedObject
			continue
		}

		// We don’t need to try deleting SBOMs that have been deleted
		if event.Type == watch.Deleted {
			continue
		}

		imageHash := obj.ObjectMeta.Name

		_, imageHashOk := wh.iwMap.Load(imageHash)
		if !imageHashOk {
			err := wh.storageClient.SpdxV1beta1().SBOMSPDXv2p3s(obj.ObjectMeta.Namespace).Delete(context.TODO(), obj.ObjectMeta.Name, v1.DeleteOptions{})
			if err != nil {
				errorCh <- err
			}
			continue
		}
	}
}

func (wh *WatchHandler) getSBOMWatcher() (watch.Interface, error) {
	return wh.storageClient.SpdxV1beta1().SBOMSPDXv2p3s("").Watch(context.TODO(), v1.ListOptions{})
}

// watch for sbom changes, and trigger scans accordingly
func (wh *WatchHandler) SBOMWatch(ctx context.Context, sessionObjChan *chan utils.SessionObj) {
	inputEvents := make(chan watch.Event)
	commands := make(chan *apis.Command)
	errorCh := make(chan error)
	sbomEvents := make(<-chan watch.Event)

	// The watcher is considered unavailable by default
	sbomWatcherUnavailable := make(chan struct{})
	go func() {
		sbomWatcherUnavailable <- struct{}{}
	}()

	go wh.HandleSBOMEvents(inputEvents, errorCh)

	// notifyWatcherDown notifies the appropriate channel that the watcher
	// is down and backs off for the retry interval to not produce
	// unnecessary events
	notifyWatcherDown := func(watcherDownCh chan<- struct{}) {
		go func() { watcherDownCh <- struct{}{} }()
		time.Sleep(retryInterval)
	}

	var watcher watch.Interface
	var err error
	for {
		select {
		case sbomEvent, ok := <-sbomEvents:
			if ok {
				inputEvents <- sbomEvent
			} else {
				notifyWatcherDown(sbomWatcherUnavailable)
			}
		case cmd, ok := <-commands:
			if ok {
				utils.AddCommandToChannel(ctx, cmd, sessionObjChan)
			} else {
				notifyWatcherDown(sbomWatcherUnavailable)
			}
		case err, ok := <-errorCh:
			if ok {
				logger.L().Ctx(ctx).Error(fmt.Sprintf("error in SBOMWatch: %v", err.Error()))
			} else {
				notifyWatcherDown(sbomWatcherUnavailable)
			}
		case <-sbomWatcherUnavailable:
			if watcher != nil {
				watcher.Stop()
			}

			watcher, err = wh.getSBOMWatcher()
			if err != nil {
				notifyWatcherDown(sbomWatcherUnavailable)
			} else {
				sbomEvents = watcher.ResultChan()
			}
		}
	}
}

func (wh *WatchHandler) getSBOMFilteredWatcher() (watch.Interface, error) {
	return wh.storageClient.SpdxV1beta1().SBOMSPDXv2p3Filtereds("").Watch(context.TODO(), v1.ListOptions{})
}

// SBOMFilteredWatch watches and processes changes on Filtered SBOMs
func (wh *WatchHandler) SBOMFilteredWatch(ctx context.Context, sessionObjChan *chan utils.SessionObj) {
	inputEvents := make(chan watch.Event)
	cmdCh := make(chan *apis.Command)
	errorCh := make(chan error)
	sbomEvents := make(<-chan watch.Event)

	// The watcher is considered unavailable by default
	sbomWatcherUnavailable := make(chan struct{})
	go func() {
		sbomWatcherUnavailable <- struct{}{}
	}()

	go wh.HandleSBOMFilteredEvents(inputEvents, cmdCh, errorCh)

	// notifyWatcherDown notifies the appropriate channel that the watcher
	// is down and backs off for the retry interval to not produce
	// unnecessary events
	notifyWatcherDown := func(watcherDownCh chan<- struct{}) {
		go func() { watcherDownCh <- struct{}{} }()
		time.Sleep(retryInterval)
	}

	var watcher watch.Interface
	var err error
	for {
		select {
		case sbomEvent, ok := <-sbomEvents:
			if ok {
				inputEvents <- sbomEvent
			} else {
				notifyWatcherDown(sbomWatcherUnavailable)
			}
		case cmd, ok := <-cmdCh:
			if ok {
				utils.AddCommandToChannel(ctx, cmd, sessionObjChan)
			} else {
				notifyWatcherDown(sbomWatcherUnavailable)
			}
		case err, ok := <-errorCh:
			if ok {
				logger.L().Ctx(ctx).Error(fmt.Sprintf("error in SBOMFilteredWatch: %v", err.Error()))
			} else {
				notifyWatcherDown(sbomWatcherUnavailable)
			}
		case <-sbomWatcherUnavailable:
			if watcher != nil {
				watcher.Stop()
			}

			watcher, err = wh.getSBOMFilteredWatcher()
			if err != nil {
				notifyWatcherDown(sbomWatcherUnavailable)
			} else {
				sbomEvents = watcher.ResultChan()
			}
		}
	}
}

// watch for pods changes, and trigger scans accordingly
func (wh *WatchHandler) PodWatch(ctx context.Context, sessionObjChan *chan utils.SessionObj) {
	logger.L().Ctx(ctx).Debug("starting pod watch")
	for {
		podsWatch, err := wh.getPodWatcher()
		if err != nil {
			logger.L().Ctx(ctx).Error(fmt.Sprintf("error to getPodWatcher, err :%s", err.Error()), helpers.Error(err))
			time.Sleep(retryInterval)
			continue
		}
		wh.handlePodWatcher(ctx, podsWatch, sessionObjChan)
	}
}

func (wh *WatchHandler) cleanUpInstanceIDs() {
	wh.instanceIDsMutex.Lock()
	wh.hashedInstanceIDs = []string{}
	wh.instanceIDsMutex.Unlock()
}

func (wh *WatchHandler) cleanUpIDs() {
	wh.iwMap.Clear()
	wh.cleanUpInstanceIDs()
	wh.cleanUpWlidsToContainerToImageIDMap()
}

func (wh *WatchHandler) cleanUpWlidsToContainerToImageIDMap() {
	wh.wlidsToContainerToImageIDMapMutex.Lock()
	defer wh.wlidsToContainerToImageIDMapMutex.Unlock()

	wh.wlidsToContainerToImageIDMap = make(WlidsToContainerToImageIDMap)
}

func (wh *WatchHandler) GetWlidsForImageHash(imageHash string) []string {
	wlids, ok := wh.iwMap.Load(imageHash)
	if !ok {
		return []string{}
	}
	return wlids
}

func (wh *WatchHandler) GetContainerToImageIDForWlid(wlid string) map[string]string {
	wh.wlidsToContainerToImageIDMapMutex.RLock()
	defer wh.wlidsToContainerToImageIDMapMutex.RUnlock()

	containerToImageIds, ok := wh.wlidsToContainerToImageIDMap[wlid]
	if !ok {
		return map[string]string{}
	}
	return containerToImageIds
}

func (wh *WatchHandler) addToInstanceIDsList(instanceID instanceidhandler.IInstanceID) {
	wh.instanceIDsMutex.Lock()
	defer wh.instanceIDsMutex.Unlock()
	h := instanceID.GetHashed()

	if !slices.Contains(wh.hashedInstanceIDs, h) {
		wh.hashedInstanceIDs = append(wh.hashedInstanceIDs, h)
	}
}

func (wh *WatchHandler) addToImageIDToWlidsMap(imageID string, wlids ...string) {
	if len(wlids) == 0 {
		return
	}
	imageID, _ = extractImageHash(imageID)
	wh.iwMap.Add(imageID, wlids...)
}

func (wh *WatchHandler) addToWlidsToContainerToImageIDMap(wlid string, containerName string, imageID string) {
	wh.wlidsToContainerToImageIDMapMutex.Lock()
	defer wh.wlidsToContainerToImageIDMapMutex.Unlock()

	if _, ok := wh.wlidsToContainerToImageIDMap[wlid]; !ok {
		wh.wlidsToContainerToImageIDMap[wlid] = make(map[string]string)
	}

	wh.wlidsToContainerToImageIDMap[wlid][containerName] = imageID
}

func (wh *WatchHandler) buildIDs(ctx context.Context, podList *core1.PodList) {
	for i := range podList.Items {

		if podList.Items[i].Status.Phase != core1.PodRunning {
			continue
		}

		podList.Items[i].APIVersion = "v1"
		podList.Items[i].Kind = "Pod"

		//check if at least one container is  running
		hasOneContainerRunning := false
		for _, containerStatus := range podList.Items[i].Status.ContainerStatuses {
			if containerStatus.State.Running != nil {
				hasOneContainerRunning = true
				break
			}
		}

		if !hasOneContainerRunning {
			continue
		}

		wl, err := wh.getParentWorkloadForPod(&podList.Items[i])
		if err != nil {
			logger.L().Ctx(ctx).Error("Failed to get parent ID for pod", helpers.String("pod", podList.Items[i].Name), helpers.String("namespace", podList.Items[i].Namespace), helpers.Error(err))
			continue
		}

		parentWlid := pkgwlid.GetWLID(utils.ClusterConfig.ClusterName, wl.GetNamespace(), wl.GetKind(), wl.GetName())

		imgIDsToContainers := extractImageIDsToContainersFromPod(&podList.Items[i])

		instanceID, err := instanceidhandlerv1.GenerateInstanceIDFromPod(&podList.Items[i])
		if err != nil {
			logger.L().Ctx(ctx).Error("Failed to generate instance ID for pod", helpers.String("pod", podList.Items[i].Name), helpers.String("namespace", podList.Items[i].Namespace), helpers.Error(err))
			continue
		}

		for i := range instanceID {
			wh.addToInstanceIDsList(instanceID[i])
		}

		for imgID, containers := range imgIDsToContainers {
			wh.addToImageIDToWlidsMap(imgID, parentWlid)
			for _, containerName := range containers {
				wh.addToWlidsToContainerToImageIDMap(parentWlid, containerName, imgID)
			}
		}
	}
}

// returns a watcher watching from current resource version
func (wh *WatchHandler) getPodWatcher() (watch.Interface, error) {
	podsWatch, err := wh.k8sAPI.KubernetesClient.CoreV1().Pods("").Watch(context.TODO(), v1.ListOptions{
		ResourceVersion: wh.currentPodListResourceVersion,
	})
	if err != nil {
		return nil, err
	}

	return podsWatch, nil
}

func (wh *WatchHandler) restartResourceVersion(podWatch watch.Interface) error {
	podWatch.Stop()
	return wh.updateResourceVersion()
}

func (wh *WatchHandler) updateResourceVersion() error {
	podsList, err := wh.k8sAPI.ListPods("", map[string]string{})
	if err != nil {
		return err
	}
	wh.currentPodListResourceVersion = podsList.GetResourceVersion()
	return nil
}

// returns a map of <imageID> : <containerName> for imageIDs in pod that are not in the map
func (wh *WatchHandler) getNewContainerToImageIDsFromPod(pod *core1.Pod) map[string]string {
	newContainerToImageIDs := make(map[string]string)
	imageIDsToContainers := extractImageIDsToContainersFromPod(pod)

	for imageID, containers := range imageIDsToContainers {
		for _, container := range containers {
			imageIDHashed, _ := extractImageHash(imageID)
			if _, imageIDinMap := wh.iwMap.Load(imageIDHashed); !imageIDinMap {
				newContainerToImageIDs[container] = imageID
			}
		}
	}

	return newContainerToImageIDs
}

// returns pod and true if event status is modified, pod is exists and is running
func (wh *WatchHandler) getPodFromEventIfRunning(ctx context.Context, event watch.Event) (*core1.Pod, bool) {
	if event.Type != watch.Modified {
		return nil, false
	}
	var pod *core1.Pod
	if val, ok := event.Object.(*core1.Pod); ok {
		pod = val
		if pod.Status.Phase != core1.PodRunning {
			return nil, false
		}
	} else {
		logger.L().Ctx(ctx).Error("Failed to cast event object to pod", helpers.Error(fmt.Errorf("failed to cast event object to pod")))
		return nil, false
	}

	// check that Pod exists (when deleting a Pod we get MODIFIED events with Running status)
	_, err := wh.k8sAPI.GetWorkload(pod.GetNamespace(), "pod", pod.GetName())
	if err != nil {
		return nil, false
	}

	return pod, true
}

func (wh *WatchHandler) getParentIDForPod(pod *core1.Pod) (string, error) {
	pod.TypeMeta.Kind = "Pod"
	podMarshalled, err := json.Marshal(pod)
	if err != nil {
		return "", err
	}
	wl, err := workloadinterface.NewWorkload(podMarshalled)
	if err != nil {
		return "", err
	}
	kind, name, err := wh.k8sAPI.CalculateWorkloadParentRecursive(wl)
	if err != nil {
		return "", err
	}
	return pkgwlid.GetWLID(utils.ClusterConfig.ClusterName, wl.GetNamespace(), kind, name), nil

}

func (wh *WatchHandler) getParentWorkloadForPod(pod *core1.Pod) (workloadinterface.IWorkload, error) {
	pod.TypeMeta.Kind = "Pod"
	podMarshalled, err := json.Marshal(pod)
	if err != nil {
		return nil, err
	}
	wl, err := workloadinterface.NewWorkload(podMarshalled)
	if err != nil {
		return nil, err
	}

	kind, name, err := wh.k8sAPI.CalculateWorkloadParentRecursive(wl)
	if kind == "Node" {
		return wl, nil
	}

	if err != nil {
		return nil, err
	}
	parentWorkload, err := wh.k8sAPI.GetWorkload(wl.GetNamespace(), kind, name)
	if err != nil {
		return nil, err
	}
	return parentWorkload, nil
}

func (wh *WatchHandler) handlePodWatcher(ctx context.Context, podsWatch watch.Interface, sessionObjChan *chan utils.SessionObj) {
	var err error
	for {
		event, ok := <-podsWatch.ResultChan()
		if !ok {
			err = wh.restartResourceVersion(podsWatch)
			if err != nil {
				logger.L().Ctx(ctx).Error(fmt.Sprintf("error to restartResourceVersion, err :%s", err.Error()), helpers.Error(err))
			}
			return
		}

		pod, ok := wh.getPodFromEventIfRunning(ctx, event)
		if !ok {
			continue
		}

		parentWlid, err := wh.getParentIDForPod(pod)
		if err != nil {
			logger.L().Ctx(ctx).Error(fmt.Sprintf("error to getParentIDForPod, err :%s", err.Error()), helpers.Error(err))
			continue
		}

		newContainersToImageIDs := wh.getNewContainerToImageIDsFromPod(pod)

		var cmd *apis.Command
		if len(newContainersToImageIDs) > 0 {
			// new image, add to respective maps
			for container, imgID := range newContainersToImageIDs {
				wh.addToImageIDToWlidsMap(imgID, parentWlid)
				wh.addToWlidsToContainerToImageIDMap(parentWlid, container, imgID)
			}
			// new image, trigger SBOM
			cmd = getImageScanCommand(parentWlid, newContainersToImageIDs)
		} else {
			// old image
			if wh.isWlidInMap(parentWlid) {
				// old workload, no need to trigger CVE
				continue
			}
			// new workload, trigger CVE
			containersToImageIds := utils.ExtractContainersToImageIDsFromPod(pod)
			for container, imgID := range containersToImageIds {
				wh.addToWlidsToContainerToImageIDMap(parentWlid, container, imgID)
			}
			cmd = getImageScanCommand(parentWlid, containersToImageIds)
		}

		// generate instance IDs
		instanceID, err := instanceidhandlerv1.GenerateInstanceIDFromPod(pod)
		if err != nil {
			logger.L().Ctx(ctx).Error("Failed to generate instance ID for pod", helpers.String("pod", pod.GetName()), helpers.String("namespace", pod.GetNamespace()), helpers.Error(err))
			continue
		}

		// save on map
		for i := range instanceID {
			wh.addToInstanceIDsList(instanceID[i])
		}

		utils.AddCommandToChannel(ctx, cmd, sessionObjChan)
	}
}

func (wh *WatchHandler) isWlidInMap(wlid string) bool {
	wh.wlidsToContainerToImageIDMapMutex.RLock()
	defer wh.wlidsToContainerToImageIDMapMutex.RUnlock()

	_, ok := wh.wlidsToContainerToImageIDMap[wlid]
	return ok
}
