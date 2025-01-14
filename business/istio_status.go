package business

import (
	"fmt"
	"sync"
	"time"

	apps_v1 "k8s.io/api/apps/v1"
	core_v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/kiali/kiali/config"
	"github.com/kiali/kiali/kubernetes"
	"github.com/kiali/kiali/log"
	"github.com/kiali/kiali/models"
	"github.com/kiali/kiali/util/httputil"
)

// SvcService deals with fetching istio/kubernetes services related content and convert to kiali model
type IstioStatusService struct {
	k8s kubernetes.ClientInterface
}

type ComponentStatus struct {
	// The app label value of the Istio component
	//
	// example: istio-ingressgateway
	// required: true
	Name string `json:"name"`

	// The status of a Istio component
	//
	// example:  Not Found
	// required: true
	Status string `json:"status"`

	// When true, the component is necessary for Istio to function. Otherwise, it is an addon
	//
	// example:  true
	// required: true
	IsCore bool `json:"is_core"`
}

type IstioComponentStatus []ComponentStatus

func (ics *IstioComponentStatus) merge(cs IstioComponentStatus) IstioComponentStatus {
	*ics = append(*ics, cs...)
	return *ics
}

const (
	Healthy     string = "Healthy"
	NotFound    string = "NotFound"
	NotReady    string = "NotReady"
	Unhealthy   string = "Unhealthy"
	Unreachable string = "Unreachable"
)

func (iss *IstioStatusService) GetStatus() (IstioComponentStatus, error) {
	if !config.Get().ExternalServices.Istio.ComponentStatuses.Enabled {
		return IstioComponentStatus{}, nil
	}

	ics, err := iss.getIstioComponentStatus()
	if err != nil {
		return nil, err
	}

	return ics.merge(iss.getAddonComponentStatus()), nil
}

func (iss *IstioStatusService) getIstioComponentStatus() (IstioComponentStatus, error) {
	// Fetching workloads from component namespaces
	ds, err := iss.getComponentNamespacesWorkloads()
	if err != nil {
		return IstioComponentStatus{}, err
	}

	deploymentStatus, err := iss.getStatusOf(ds)
	if err != nil {
		return IstioComponentStatus{}, err
	}

	istiodStatus, err := iss.getIstiodReachingCheck()
	if err != nil {
		return IstioComponentStatus{}, err
	}

	return deploymentStatus.merge(istiodStatus), nil
}

func (iss *IstioStatusService) getComponentNamespacesWorkloads() ([]apps_v1.Deployment, error) {
	var wg sync.WaitGroup

	nss := map[string]bool{}
	deps := make([]apps_v1.Deployment, 0)

	comNs := getComponentNamespaces()

	depsChan := make(chan []apps_v1.Deployment, len(comNs))
	errChan := make(chan error, len(comNs))

	for _, n := range comNs {
		if !nss[n] {
			wg.Add(1)
			nss[n] = true

			go func(n string, depsChan chan []apps_v1.Deployment, errChan chan error) {
				defer wg.Done()
				var ds []apps_v1.Deployment
				var err error
				if IsNamespaceCached(n) {
					ds, err = kialiCache.GetDeployments(n)
				} else {
					// Adding a warning to enable cache for fetching Istio Status.
					// It should use cache, as it's an intensive operation but we won't fail otherwise
					// If user doesn't have access to istio namespace AND it doesn't have enabled cache it won't get the Istio status
					log.Warningf("Kiali has not [%s] namespace cached. It is required to fetch Istio Status correctly", n)
					ds, err = iss.k8s.GetDeployments(n)
				}
				depsChan <- ds
				errChan <- err
			}(n, depsChan, errChan)
		}
	}

	wg.Wait()

	close(depsChan)
	close(errChan)
	for err := range errChan {
		if err != nil {
			return nil, err
		}
	}

	for dep := range depsChan {
		if dep != nil {
			deps = append(deps, dep...)
		}
	}

	return deps, nil
}

func getComponentNamespaces() []string {
	nss := make([]string, 0)

	// By default, add the istio control plane namespace
	nss = append(nss, config.Get().IstioNamespace)

	// Adding Istio Components namespaces
	externalServices := config.Get().ExternalServices
	for _, cmp := range externalServices.Istio.ComponentStatuses.Components {
		if cmp.Namespace != "" {
			nss = append(nss, cmp.Namespace)
		}
	}

	return nss
}

func istioCoreComponents() map[string]bool {
	components := map[string]bool{}
	cs := config.Get().ExternalServices.Istio.ComponentStatuses
	for _, c := range cs.Components {
		components[c.AppLabel] = c.IsCore
	}
	return components
}

func (iss *IstioStatusService) getStatusOf(ds []apps_v1.Deployment) (IstioComponentStatus, error) {
	statusComponents := istioCoreComponents()
	isc := IstioComponentStatus{}
	cf := map[string]bool{}

	// Map workloads there by app name
	for _, d := range ds {
		appLabel := labels.Set(d.Spec.Template.Labels).Get("app")
		if appLabel == "" {
			continue
		}

		isCore, found := statusComponents[appLabel]
		if !found {
			continue
		}

		// Component found
		cf[appLabel] = true

		if status := GetDeploymentStatus(d); status != Healthy {
			// Check status
			isc = append(isc, ComponentStatus{
				Name:   d.Name,
				Status: status,
				IsCore: isCore,
			},
			)
		}
	}

	// Add missing deployments
	componentNotFound := 0
	for comp, isCore := range statusComponents {
		if _, found := cf[comp]; !found {
			componentNotFound += 1
			isc = append(isc, ComponentStatus{
				Name:   comp,
				Status: NotFound,
				IsCore: isCore,
			})
		}
	}

	// When all the deployments are missing,
	// Warn users that their kiali config might be wrong
	if componentNotFound == len(statusComponents) {
		return isc, fmt.Errorf(
			"Kiali is unable to find any Istio deployment in namespace %s. Are you sure the Istio namespace is configured correctly in Kiali?",
			config.Get().IstioNamespace)
	}

	return isc, nil
}

func GetDeploymentStatus(d apps_v1.Deployment) string {
	status := Unhealthy
	wl := &models.Workload{}
	wl.ParseDeployment(&d)
	if wl.DesiredReplicas == 0 {
		status = NotReady
	} else if wl.DesiredReplicas == wl.AvailableReplicas && wl.DesiredReplicas == wl.CurrentReplicas {
		status = Healthy
	}
	return status
}

func (iss *IstioStatusService) getAddonComponentStatus() IstioComponentStatus {
	var wg sync.WaitGroup
	wg.Add(4)

	staChan := make(chan IstioComponentStatus, 4)
	extServices := config.Get().ExternalServices
	ics := IstioComponentStatus{}

	go getAddonStatus("prometheus", true, extServices.Prometheus.URL, &extServices.Prometheus.Auth, true, staChan, &wg)
	go getAddonStatus("grafana", extServices.Grafana.Enabled, extServices.Grafana.InClusterURL, &extServices.Grafana.Auth, extServices.Grafana.IsCoreComponent, staChan, &wg)
	go getAddonStatus("jaeger", extServices.Tracing.Enabled, extServices.Tracing.InClusterURL, &extServices.Tracing.Auth, extServices.Tracing.IsCoreComponent, staChan, &wg)

	// Custom dashboards may use the main Prometheus config
	customProm := extServices.CustomDashboards.Prometheus
	if customProm.URL == "" {
		customProm = extServices.Prometheus
	}
	go getAddonStatus("custom dashboards", extServices.CustomDashboards.Enabled, customProm.URL, &customProm.Auth, extServices.CustomDashboards.IsCoreComponent, staChan, &wg)

	wg.Wait()

	close(staChan)
	for stat := range staChan {
		ics.merge(stat)
	}

	return ics
}

func (iss *IstioStatusService) getIstiodReachingCheck() (IstioComponentStatus, error) {
	cfg := config.Get()

	istiods, err := iss.k8s.GetPods(cfg.IstioNamespace, labels.Set(map[string]string{"app": "istiod"}).String())
	if err != nil {
		return nil, err
	}

	healthyIstiods := make([]*core_v1.Pod, 0, len(istiods))
	for i, istiod := range istiods {
		if istiod.Status.Phase == "Running" {
			healthyIstiods = append(healthyIstiods, &istiods[i])
		}
	}

	wg := sync.WaitGroup{}
	wg.Add(len(healthyIstiods))
	syncChan := make(chan ComponentStatus, len(healthyIstiods))

	for _, istiod := range healthyIstiods {
		go func(name, namespace string) {
			defer wg.Done()
			// Using the proxy method to make sure that K8s API has access to the Istio Control Plane namespace.
			// By proxying one Istiod, we ensure that the following connection is allowed:
			// Kiali -> K8s API (proxy) -> istiod
			// This scenario is no obvious for private clusters (like GKE private cluster)
			_, err := iss.k8s.GetPodProxy(namespace, name, "/ready")
			if err != nil {
				syncChan <- ComponentStatus{
					Name:   name,
					Status: Unreachable,
					IsCore: true,
				}
			}
		}(istiod.Name, istiod.Namespace)
	}

	wg.Wait()
	close(syncChan)
	ics := IstioComponentStatus{}
	for componentStatus := range syncChan {
		ics.merge(IstioComponentStatus{componentStatus})
	}

	return ics, nil
}

func getAddonStatus(name string, enabled bool, url string, auth *config.Auth, isCore bool, staChan chan<- IstioComponentStatus, wg *sync.WaitGroup) {
	defer wg.Done()

	if auth.UseKialiToken {
		token, err := kubernetes.GetKialiToken()
		if err != nil {
			log.Errorf("Could not read the Kiali Service Account token: %v", err)
		}
		auth.Token = token
	}

	// When the addOn is disabled, don't perform any check
	if !enabled {
		return
	}

	// Call the addOn service endpoint to find out whether is reachable or not
	_, statusCode, err := httputil.HttpGet(url, auth, 10*time.Second)
	if err != nil || statusCode > 399 {
		staChan <- IstioComponentStatus{
			ComponentStatus{
				Name:   name,
				Status: Unreachable,
				IsCore: isCore,
			},
		}
	}
}
