/*
Copyright 2017 The Kubernetes Authors.

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
	"time"

	"github.com/golang/glog"
	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	vpa_types "k8s.io/autoscaler/vertical-pod-autoscaler/pkg/apis/poc.autoscaling.k8s.io/v1alpha1"
	vpa_clientset "k8s.io/autoscaler/vertical-pod-autoscaler/pkg/client/clientset/versioned"
	vpa_api "k8s.io/autoscaler/vertical-pod-autoscaler/pkg/client/clientset/versioned/typed/poc.autoscaling.k8s.io/v1alpha1"
	vpa_lister "k8s.io/autoscaler/vertical-pod-autoscaler/pkg/client/listers/poc.autoscaling.k8s.io/v1alpha1"
	vpa_api_util "k8s.io/autoscaler/vertical-pod-autoscaler/pkg/utils/vpa"
	"k8s.io/autoscaler/vertical-pod-autoscaler/recommender/cluster"
	"k8s.io/autoscaler/vertical-pod-autoscaler/recommender/logic"
	"k8s.io/autoscaler/vertical-pod-autoscaler/recommender/model"
	"k8s.io/autoscaler/vertical-pod-autoscaler/recommender/signals"
	kube_client "k8s.io/client-go/kubernetes"
	v1lister "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	resourceclient "k8s.io/metrics/pkg/client/clientset_generated/clientset/typed/metrics/v1beta1"
)

// Recommender recommend resources for certain containers, based on utilization periodically got from metrics api.
type Recommender interface {
	Run()
}

type recommender struct {
	clusterState            *model.ClusterState
	specClient              cluster.SpecClient
	metricsClient           cluster.MetricsClient
	metricsFetchingInterval time.Duration
	historyProvider         signals.HistoryProvider
	vpaClient               vpa_api.VerticalPodAutoscalerInterface
	vpaLister               vpa_lister.VerticalPodAutoscalerLister
	podResourceRecommender  logic.PodResourceRecommender
}

func (r *recommender) readHistory() {
	clusterHistory, err := r.historyProvider.GetClusterHistory()
	if err != nil {
		glog.Errorf("Cannot get cluster history: %v", err)
	}
	for podID, podHistory := range clusterHistory {
		glog.V(4).Infof("Adding pod %v with labels %v", podID, podHistory.LastLabels)
		r.clusterState.AddOrUpdatePod(podID, podHistory.LastLabels)
		for containerName, sampleList := range podHistory.Samples {
			containerID := model.ContainerID{
				PodID:         podID,
				ContainerName: containerName}
			glog.V(4).Infof("Adding %d samples for container %v", len(sampleList), containerID)
			for _, sample := range sampleList {
				r.clusterState.AddSample(
					&model.ContainerUsageSampleWithKey{
						ContainerUsageSample: sample,
						Container:            containerID})
			}
		}
	}
}

// Fetch VPA objects and load them into the cluster state.
func (r *recommender) loadVPAs() {
	vpaCRDs, err := r.vpaLister.List(labels.Everything())
	if err != nil {
		glog.Errorf("Cannot list VPAs. Reason: %+v", err)
	} else {
		glog.V(3).Infof("Fetched VPAs.")
	}

	vpaNameToSelector := make(map[model.VpaID]labels.Selector)
	for n, vpaCRD := range vpaCRDs {
		glog.V(3).Infof("VPA CRD #%v: %+v", n, vpaCRD)
		selector, err := metav1.LabelSelectorAsSelector(vpaCRD.Spec.Selector)
		if err != nil {
			glog.Errorf(
				"Cannot convert VPA Selector %+v to internal representation. Reason: %+v",
				vpaCRD.Spec.Selector, err)
		} else {
			vpaName := vpaCRD.ObjectMeta.Name
			vpaNameToSelector[model.VpaID{VpaName: vpaName}] = selector
			if vpaCRD.Status.LastUpdateTime.IsZero() {
				glog.V(3).Infof("Empty status in %v, initializing", vpaName)
				_, err := vpa_api_util.InitVpaStatus(r.vpaClient, vpaName)
				if err != nil {
					glog.Errorf(
						"Cannot initialize VPA %v. Reason: %+v", vpaName, err)
				}
			}
		}
	}
	for key := range r.clusterState.Vpas {
		if _, exists := vpaNameToSelector[key]; !exists {
			glog.V(3).Infof("Deleting VPA %v", key)
			r.clusterState.DeleteVpa(key)
		}
	}
	for key, selector := range vpaNameToSelector {
		r.clusterState.AddOrUpdateVpa(key, selector.String())
	}
}

// Load pod into the cluster state.
func (r *recommender) loadPods() {
	podSpecs, err := r.specClient.GetPodSpecs()
	if err != nil {
		glog.Errorf("Cannot get SimplePodSpecs. Reason: %+v", err)
	}
	pods := make(map[model.PodID]*model.BasicPodSpec)
	for n, spec := range podSpecs {
		glog.V(3).Infof("SimplePodSpec #%v: %+v", n, spec)
		pods[spec.ID] = spec
	}
	for key := range r.clusterState.Pods {
		if _, exists := pods[key]; !exists {
			glog.V(3).Infof("Deleting Pod %v", key)
			r.clusterState.DeletePod(key)
		}
	}
	for _, pod := range pods {
		r.clusterState.AddOrUpdatePod(pod.ID, pod.PodLabels)
		for _, container := range pod.Containers {
			r.clusterState.AddOrUpdateContainer(container.ID)
		}
	}
}

// Updates VPA CRD objects' statuses.
func (r *recommender) updateVPAs() {
	for key, vpa := range r.clusterState.Vpas {
		glog.V(3).Infof("VPA to update #%v: %+v", key, vpa)

		resources := r.podResourceRecommender.GetRecommendedPodResources(vpa)
		vpaName := vpa.ID.VpaName

		containerResources := make([]vpa_types.RecommendedContainerResources, 0, len(resources))
		for containerId, res := range resources {
			containerResources = append(containerResources, vpa_types.RecommendedContainerResources{
				Name:           containerId,
				Target:         model.ResoucesAsResourceList(res.Target),
				MinRecommended: model.ResoucesAsResourceList(res.MinRecommended),
				MaxRecommended: model.ResoucesAsResourceList(res.MaxRecommended),
			})

		}

		recommendation := vpa_types.RecommendedPodResources{containerResources}
		_, err := vpa_api_util.UpdateVpaRecommendation(r.vpaClient, vpaName, recommendation)
		if err != nil {
			glog.Errorf(
				"Cannot update VPA %v object. Reason: %+v", vpaName, err)
		}
	}

}

// Currently it just prints out current utilization to the console.
// It will be soon replaced by something more useful.
func (r *recommender) runOnce() {
	glog.V(3).Infof("Recommender Run")
	r.loadVPAs()
	r.loadPods()

	metricsSnapshots, err := r.metricsClient.GetContainersMetrics()
	if err != nil {
		glog.Errorf("Cannot get containers metrics. Reason: %+v", err)
	}
	for n, snap := range metricsSnapshots {
		glog.V(3).Infof("ContainerMetricsSnapshot #%v: %+v", n, snap)
	}
	r.updateVPAs()
}

func (r *recommender) Run() {
	r.readHistory()
	for {
		select {
		case <-time.After(r.metricsFetchingInterval):
			{
				r.runOnce()
			}
		}
	}
}

func createPodResourceRecommender() logic.PodResourceRecommender {
	// Create a fake recommender that returns a hard-coded recommendation.
	// TODO: Replace with a real recommender based on past usage.
	var MiB float64 = 1024 * 1024
	target := model.Resources{
		model.ResourceCPU:    model.CPUAmountFromCores(0.5),
		model.ResourceMemory: model.MemoryAmountFromBytes(200. * MiB),
	}
	lowerBound := model.Resources{
		model.ResourceCPU:    model.CPUAmountFromCores(0.4),
		model.ResourceMemory: model.MemoryAmountFromBytes(150. * MiB),
	}
	upperBound := model.Resources{
		model.ResourceCPU:    model.CPUAmountFromCores(0.6),
		model.ResourceMemory: model.MemoryAmountFromBytes(250. * MiB),
	}
	return logic.NewPodResourceRecommender(
		logic.NewConstEstimator(target),
		logic.NewConstEstimator(lowerBound),
		logic.NewConstEstimator(upperBound))
}

// NewRecommender creates a new recommender instance,
// which can be run in order to provide continuous resource recommendations for containers.
// It requires cluster configuration object and duration between recommender intervals.
func NewRecommender(namespace string, config *rest.Config, metricsFetcherInterval time.Duration, historyProvider signals.HistoryProvider) Recommender {
	recommender := &recommender{
		clusterState:            model.NewClusterState(),
		specClient:              newSpecClient(config),
		metricsClient:           newMetricsClient(config),
		metricsFetchingInterval: metricsFetcherInterval,
		historyProvider:         historyProvider,
		vpaClient:               vpa_clientset.NewForConfigOrDie(config).PocV1alpha1().VerticalPodAutoscalers(namespace),
		vpaLister:               vpa_api_util.NewAllVpasLister(vpa_clientset.NewForConfigOrDie(config), make(chan struct{})),
		podResourceRecommender:  createPodResourceRecommender(),
	}
	glog.V(3).Infof("New Recommender created %+v", recommender)

	return recommender
}

func newSpecClient(config *rest.Config) cluster.SpecClient {
	kubeClient := kube_client.NewForConfigOrDie(config)
	podLister := newPodLister(kubeClient)
	return cluster.NewSpecClient(podLister)
}

func newMetricsClient(config *rest.Config) cluster.MetricsClient {
	metricsGetter := resourceclient.NewForConfigOrDie(config)
	return cluster.NewMetricsClient(metricsGetter)
}

// Creates PodLister, listing only not terminated pods.
func newPodLister(kubeClient kube_client.Interface) v1lister.PodLister {
	selector := fields.ParseSelectorOrDie("status.phase!=" + string(apiv1.PodPending) + ",status.phase!=" + string(apiv1.PodUnknown))
	podListWatch := cache.NewListWatchFromClient(kubeClient.CoreV1().RESTClient(), "pods", apiv1.NamespaceAll, selector)
	store := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	podLister := v1lister.NewPodLister(store)
	podReflector := cache.NewReflector(podListWatch, &apiv1.Pod{}, store, time.Hour)
	stopCh := make(chan struct{})
	go podReflector.Run(stopCh)
	return podLister
}
