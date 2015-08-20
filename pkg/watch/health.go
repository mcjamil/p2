package watch

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"time"

	"github.com/square/p2/pkg/health"
	"github.com/square/p2/pkg/kp"
	"github.com/square/p2/pkg/logging"
	"github.com/square/p2/pkg/pods"
	"github.com/square/p2/pkg/preparer"
)

// These constants should probably all be something the p2 user can set
// in their preparer config...

// Duration between reality store checks
const POLL_KV_FOR_PODS = 3 * time.Second

// Duration between health checks
const HEALTHCHECK_INTERVAL = 1 * time.Second

// Contains method for watching the consul reality store to
// track services running on a node. A manager method:
// MonitorPodHealth tracks the reality store and manages
// a health checking go routine for each service in the
// reality store

// PodWatch houses a pod's manifest, a channel to kill the
// pod's goroutine if the pod is removed from the reality
// tree, and a bool that indicates whether or not the pod
// has a running MonitorHealth go routine
type PodWatch struct {
	manifest      pods.Manifest
	updater       kp.HealthUpdater
	statusChecker StatusChecker

	// For tracking/controlling the go routine that performs health checks
	// on the pod associated with this PodWatch
	shutdownCh chan bool

	logger *logging.Logger
}

// StatusChecker holds all the data required to perform
// a status check on a particular service (ID corresponds
// to service name to be consistent with pods.Manifest).
type StatusChecker struct {
	ID     string
	Node   string
	URI    string
	Client *http.Client
}

// MonitorPodHealth is meant to be a long running go routine.
// MonitorPodHealth reads from a consul store to determine which
// services should be running on the host. MonitorPodHealth
// runs a CheckHealth routine to monitor the health of each
// service and kills routines for services that should no
// longer be running.
func MonitorPodHealth(config *preparer.PreparerConfig, logger *logging.Logger, shutdownCh chan struct{}) {
	store, err := config.GetStore()
	if err != nil {
		// A bad config should have already produced a nice, user-friendly error message.
		logger.WithError(err).Fatalln("error creating health monitor KV store")
	}
	healthManager := store.NewHealthManager(config.NodeName, *logger)
	// if GetClient fails it means the certfile/keyfile/cafile were
	// invalid or did not exist. It makes sense to throw a fatal error
	client, err := config.GetClient()
	if err != nil {
		logger.WithError(err).Fatalln("failed to get http client for this preparer")
	}

	node := config.NodeName
	pods := []PodWatch{}
	pods = updateHealthMonitors(store, healthManager, client, pods, node, logger)
	for {
		select {
		case <-time.After(POLL_KV_FOR_PODS):
			// check if pods have been added or removed
			// starts monitor routine for new pods
			// kills monitor routine for removed pods
			pods = updateHealthMonitors(store, healthManager, client, pods, node, logger)
		case <-shutdownCh:
			for _, pod := range pods {
				pod.shutdownCh <- true
			}
			healthManager.Close()
			return
		}
	}
}

// Determines what pods should be running (by checking reality store)
// Creates new PodWatch for any pod not being monitored and kills
// PodWatches of pods that have been removed from the reality store
func updateHealthMonitors(
	store kp.Store,
	healthManager kp.HealthManager,
	client *http.Client,
	watchedPods []PodWatch,
	node string,
	logger *logging.Logger,
) []PodWatch {
	path := kp.RealityPath(node)
	reality, _, err := store.ListPods(path)
	if err != nil {
		logger.WithError(err).Warningln("failed to get pods from reality store")
	}

	return updatePods(healthManager, client, watchedPods, reality, node, logger)
}

// compares services being monitored with services that
// need to be monitored.
func updatePods(
	healthManager kp.HealthManager,
	client *http.Client,
	current []PodWatch,
	reality []kp.ManifestResult,
	node string,
	logger *logging.Logger,
) []PodWatch {
	newCurrent := []PodWatch{}
	// for pod in current if pod not in reality: kill
	for _, pod := range current {
		inReality := false
		for _, man := range reality {
			if man.Manifest.Id == pod.manifest.Id {
				inReality = true
				break
			}
		}

		// if this podwatch is not in the reality store kill its go routine
		// else add this podwatch to newCurrent
		if inReality == false {
			pod.shutdownCh <- true
		} else {
			newCurrent = append(newCurrent, pod)
		}
	}
	// for pod in reality if pod not in current: create podwatch and
	// append to current
	for _, man := range reality {
		missing := true
		for _, pod := range newCurrent {
			if man.Manifest.Id == pod.manifest.Id {
				missing = false
				break
			}
		}

		// if a manifest is in reality but not current a podwatch is created
		// with that manifest and added to newCurrent
		if missing && man.Manifest.StatusPort != 0 {
			sc := StatusChecker{
				ID:     man.Manifest.Id,
				Node:   node,
				Client: client,
			}
			if man.Manifest.StatusHTTP {
				sc.URI = fmt.Sprintf("http://%s:%d/_status", node, man.Manifest.StatusPort)
			} else {
				sc.URI = fmt.Sprintf("https://%s:%d/_status", node, man.Manifest.StatusPort)
			}
			newPod := PodWatch{
				manifest:      man.Manifest,
				updater:       healthManager.NewUpdater(man.Manifest.Id, man.Manifest.Id),
				statusChecker: sc,
				shutdownCh:    make(chan bool, 1),
				logger:        logger,
			}

			// Each health monitor will have its own statusChecker
			go newPod.MonitorHealth()
			newCurrent = append(newCurrent, newPod)
		}
	}
	return newCurrent
}

// Monitor Health is a go routine that runs as long as the
// service it is monitoring. Every HEALTHCHECK_INTERVAL it
// performs a health check and writes that information to
// consul
func (p *PodWatch) MonitorHealth() {
	for {
		select {
		case <-time.After(HEALTHCHECK_INTERVAL):
			p.checkHealth()
		case <-p.shutdownCh:
			p.updater.Close()
			return
		}
	}
}

func (p *PodWatch) checkHealth() {
	health, err := p.statusChecker.Check()
	if err != nil {
		p.logger.WithError(err).Warningln("health check failed")
		return
	}

	p.updater.PutHealth(resToKPRes(health))
}

// Given the result of a status check this method
// creates a health.Result for that node/service/result
func (sc *StatusChecker) Check() (health.Result, error) {
	return sc.resultFromCheck(sc.StatusCheck())
}

func (sc *StatusChecker) resultFromCheck(resp *http.Response, err error) (health.Result, error) {
	res := health.Result{
		ID:      sc.ID,
		Node:    sc.Node,
		Service: sc.ID,
	}
	if err != nil || resp == nil {
		res.Status = health.Critical
		if err != nil {
			res.Output = err.Error()
		}
		return res, nil
	}

	res.Output, err = getBody(resp)
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		res.Status = health.Passing
	} else {
		res.Status = health.Critical
	}
	return res, err
}

// Go version of http status check
func (sc *StatusChecker) StatusCheck() (*http.Response, error) {
	return sc.Client.Get(sc.URI)
}

func getBody(resp *http.Response) (string, error) {
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func resToKPRes(res health.Result) kp.WatchResult {
	return kp.WatchResult{
		Service: res.Service,
		Node:    res.Node,
		Id:      res.ID,
		Status:  string(res.Status),
		Output:  res.Output,
	}
}
