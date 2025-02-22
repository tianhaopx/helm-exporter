package main

import (
	"flag"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Masterminds/semver"

	"github.com/sstarcher/helm-exporter/config"

	cmap "github.com/orcaman/concurrent-map"

	"github.com/sirupsen/logrus"
	log "github.com/sirupsen/logrus"

	"os"

	// Import to initialize client auth plugins.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/cli"

	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"

	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/facebookgo/flagenv"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	settings = cli.New()
	clients  = cmap.New()

	mutex = sync.RWMutex{}

	statsInfo      *prometheus.GaugeVec
	statsTimestamp *prometheus.GaugeVec
	statsOutdated  *prometheus.GaugeVec

	namespaces         = flag.String("namespaces", "", "namespaces to monitor.  Defaults to all")
	namespacesIgnore   = flag.String("namespaces-ignore", "", "namespaces to ignore.  Defaults to none")
	namespacesIgnoreRe []regexp.Regexp
	configFile         = flag.String("config", "", "Configfile to load for helm overwrite registries.  Default is empty")

	intervalDuration = flag.String("interval-duration", "0", "Enable metrics gathering in background, each given duration. If not provided, the helm stats are computed synchronously.  Default is 0")

	infoMetric      = flag.Bool("info-metric", true, "Generate info metric.  Defaults to true")
	timestampMetric = flag.Bool("timestamp-metric", true, "Generate timestamps metric.  Defaults to true")
	outdatedMetric  = flag.Bool("outdated-metric", true, "Generate version outdated metric.  Defaults to true")

	fetchLatest = flag.Bool("latest-chart-version", true, "Attempt to fetch the latest chart version from registries. Defaults to true")

	verbose = flag.Bool("verbose", false, "Enables debug logging. Defaults to false")

	statusCodeMap = map[string]float64{
		"unknown":          0,
		"deployed":         1,
		"uninstalled":      2,
		"superseded":       3,
		"failed":           -1,
		"uninstalling":     5,
		"pending-install":  6,
		"pending-upgrade":  7,
		"pending-rollback": 8,
	}

	prometheusHandler = promhttp.Handler()
)

func configureMetrics() (info *prometheus.GaugeVec, timestamp *prometheus.GaugeVec, outdated *prometheus.GaugeVec) {
	if *infoMetric == true {
		info = prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "helm_chart_info",
			Help: "Information on helm releases",
		}, []string{
			"chart",
			"release",
			"version",
			"appVersion",
			"revision",
			"updated",
			"namespace",
			"latestVersion",
			"description"})
	}

	if *timestampMetric == true {
		timestamp = prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "helm_chart_timestamp",
			Help: "Timestamps of helm releases",
		}, []string{
			"chart",
			"release",
			"version",
			"appVersion",
			"updated",
			"namespace",
			"latestVersion"})
	}

	if *outdatedMetric == true {
		outdated = prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "helm_chart_outdated",
			Help: "Outdated helm versions of helm releases",
		}, []string{
			"chart",
			"release",
			"version",
			"namespace",
			"latestVersion"})
	}

	return
}

func runStats(config config.Config, info *prometheus.GaugeVec, timestamp *prometheus.GaugeVec, outdated *prometheus.GaugeVec) {
	if info != nil {
		info.Reset()
	}
	if timestamp != nil {
		timestamp.Reset()
	}

	if outdated != nil {
		outdated.Reset()
	}

	for _, client := range clients.Items() {
		list := action.NewList(client.(*action.Configuration))
		items, err := list.Run()
		if err != nil {
			log.Warnf("got error while listing %v", err)
			continue
		}

		for _, item := range items {
			chart := item.Chart.Name()
			releaseName := item.Name
			version := item.Chart.Metadata.Version
			appVersion := item.Chart.AppVersion()
			updated := item.Info.LastDeployed.Unix() * 1000
			namespace := item.Namespace
			status := statusCodeMap[item.Info.Status.String()]
			revision := item.Version
			description := item.Info.Description
			latestVersion := ""

			if *fetchLatest {
				latestVersion = config.HelmRegistries.GetLatestVersionFromHelm(item.Chart.Name())
			}

			lv, err := semver.NewVersion(latestVersion)
			if err == nil {
				log.WithField("chart", chart).WithField("version", version).WithField("latest", latestVersion).Debug("Comparing versions")
				lc, err := semver.NewConstraint(">" + version)
				if err == nil {
					a := lc.Check(lv)
					if a {
						if outdated != nil {
							outdated.WithLabelValues(chart, releaseName, version, namespace, latestVersion).Set(1)
						}
					}
				} else {
					log.WithField("chart", chart).WithField("version", version).WithField("latest", latestVersion).Error("%s", err)
				}
			}

			if info != nil {
				info.WithLabelValues(chart, releaseName, version, appVersion, strconv.FormatInt(int64(revision), 10), strconv.FormatInt(updated, 10), namespace, latestVersion, description).Set(status)
			}
			if timestamp != nil {
				timestamp.WithLabelValues(chart, releaseName, version, appVersion, strconv.FormatInt(updated, 10), namespace, latestVersion).Set(float64(updated))
			}
		}
	}
}

func runStatsPeriodically(interval time.Duration, config config.Config) {
	for {
		info, timestamp, outdated := configureMetrics()
		runStats(config, info, timestamp, outdated)
		registerMetrics(prometheus.DefaultRegisterer, info, timestamp, outdated)
		time.Sleep(interval)
	}
}

func registerMetrics(register prometheus.Registerer, info, timestamp *prometheus.GaugeVec, outdated *prometheus.GaugeVec) {
	mutex.Lock()
	defer mutex.Unlock()

	if statsInfo != nil {
		register.Unregister(statsInfo)
	}
	register.MustRegister(info)
	statsInfo = info

	if statsTimestamp != nil {
		register.Unregister(statsTimestamp)
	}
	register.MustRegister(timestamp)
	statsTimestamp = timestamp

	if statsOutdated != nil {
		register.Unregister(statsOutdated)
	}
	register.MustRegister(outdated)
	statsOutdated = outdated
}

func newHelmStatsHandler(config config.Config, synchrone bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if synchrone {
			runStats(config, statsInfo, statsTimestamp, statsOutdated)
		} else {
			mutex.RLock()
			defer mutex.RUnlock()
		}

		prometheusHandler.ServeHTTP(w, r)
	}
}

func healthz(w http.ResponseWriter, r *http.Request) {

}

func connect(namespace string) {
	actionConfig := new(action.Configuration)
	err := actionConfig.Init(settings.RESTClientGetter(), namespace, os.Getenv("HELM_DRIVER"), log.Infof)
	if err != nil {
		log.Warnf("failed to connect to %s with %v", namespace, err)
	} else {
		log.Infof("Watching namespace %s", namespace)
		clients.Set(namespace, actionConfig)
	}
}

func informer() {
	actionConfig := new(action.Configuration)
	err := actionConfig.Init(settings.RESTClientGetter(), settings.Namespace(), os.Getenv("HELM_DRIVER"), log.Infof)
	if err != nil {
		log.Fatal(err)
	}

	clientset, err := actionConfig.KubernetesClientSet()
	if err != nil {
		log.Fatal(err)
	}

	factory := informers.NewSharedInformerFactory(clientset, 0)
	informer := factory.Core().V1().Namespaces().Informer()
	stopper := make(chan struct{})
	defer close(stopper)

	informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			// "k8s.io/apimachinery/pkg/apis/meta/v1" provides an Object
			// interface that allows us to get metadata easily
			mObj := obj.(v1.Object)
			shouldConnect := true
			for _, re := range namespacesIgnoreRe {
				if re.FindString(mObj.GetName()) != "" {
					shouldConnect = false
					log.Infof("Namespace %s is in ignore list", mObj.GetName())
					break
				}
			}
			if shouldConnect {
				connect(mObj.GetName())
			}
		},
		DeleteFunc: func(obj interface{}) {
			mObj := obj.(v1.Object)
			log.Infof("Removing namespace %s", mObj.GetName())
			clients.Remove(mObj.GetName())
		},
	})

	informer.Run(stopper)
}

func main() {
	flagenv.Parse()
	flag.Parse()

	if *verbose == true {
		logrus.SetLevel(logrus.DebugLevel)
	}

	config := config.New(*configFile)
	runIntervalDuration, err := time.ParseDuration(*intervalDuration)
	if err != nil {
		log.Fatalf("invalid duration `%s`: %s", *intervalDuration, err)
	}

	for _, listItem := range strings.Split(*namespacesIgnore, ",") {
		re, err := regexp.Compile(listItem)
		if err != nil {
			log.Infof("Regexp error : %s", err)
		} else {
			namespacesIgnoreRe = append(namespacesIgnoreRe, *re)
		}
	}

	if namespaces == nil || *namespaces == "" {
		go informer()
	} else {
		for _, namespace := range strings.Split(*namespaces, ",") {
			connect(namespace)
		}
	}

	if runIntervalDuration != 0 {
		go runStatsPeriodically(runIntervalDuration, config)
	} else {
		info, timestamp, outdated := configureMetrics()
		registerMetrics(prometheus.DefaultRegisterer, info, timestamp, outdated)
	}

	http.HandleFunc("/metrics", newHelmStatsHandler(config, runIntervalDuration == 0))
	http.HandleFunc("/healthz", healthz)
	log.Fatal(http.ListenAndServe(":9571", nil))
}
