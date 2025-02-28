// Copyright 2021 Redpanda Data, Inc.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.md
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/http/pprof"
	"os"
	"strings"
	"time"

	cmapiv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	helmControllerAPIv2beta1 "github.com/fluxcd/helm-controller/api/v2beta1"
	helmController "github.com/fluxcd/helm-controller/shim"
	"github.com/fluxcd/pkg/runtime/client"
	helper "github.com/fluxcd/pkg/runtime/controller"
	"github.com/fluxcd/pkg/runtime/events"
	"github.com/fluxcd/pkg/runtime/logger"
	"github.com/fluxcd/pkg/runtime/metrics"
	sourceControllerAPIv1 "github.com/fluxcd/source-controller/api/v1"
	sourceControllerAPIv1beta2 "github.com/fluxcd/source-controller/api/v1beta2"
	helmSourceController "github.com/fluxcd/source-controller/shim"
	flag "github.com/spf13/pflag"
	"helm.sh/helm/v3/pkg/getter"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	clusterredpandacomv1alpha1 "github.com/redpanda-data/redpanda-operator/src/go/k8s/api/cluster.redpanda.com/v1alpha1"
	redpandav1alpha1 "github.com/redpanda-data/redpanda-operator/src/go/k8s/api/redpanda/v1alpha1"
	vectorizedv1alpha1 "github.com/redpanda-data/redpanda-operator/src/go/k8s/api/vectorized/v1alpha1"
	clusterredpandacomcontrollers "github.com/redpanda-data/redpanda-operator/src/go/k8s/internal/controller/cluster.redpanda.com"
	redpandacontrollers "github.com/redpanda-data/redpanda-operator/src/go/k8s/internal/controller/redpanda"
	adminutils "github.com/redpanda-data/redpanda-operator/src/go/k8s/pkg/admin"
	consolepkg "github.com/redpanda-data/redpanda-operator/src/go/k8s/pkg/console"
	"github.com/redpanda-data/redpanda-operator/src/go/k8s/pkg/resources"
	redpandawebhooks "github.com/redpanda-data/redpanda-operator/src/go/k8s/webhooks/redpanda"
)

type RedpandaController string

type OperatorState string

func (r RedpandaController) toString() string {
	return string(r)
}

const (
	defaultConfiguratorContainerImage = "vectorized/configurator"

	AllControllers         = RedpandaController("all")
	NodeController         = RedpandaController("nodeWatcher")
	DecommissionController = RedpandaController("decommission")

	OperatorV1Mode          = OperatorState("Clustered-v1")
	OperatorV2Mode          = OperatorState("Namespaced-v2")
	ClusterControllerMode   = OperatorState("Clustered-Controllers")
	NamespaceControllerMode = OperatorState("Namespaced-Controllers")
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
	getters  = getter.Providers{
		getter.Provider{
			Schemes: []string{"http", "https"},
			New:     getter.NewHTTPGetter,
		},
		getter.Provider{
			Schemes: []string{"oci"},
			New:     getter.NewOCIGetter,
		},
	}

	clientOptions  client.Options
	kubeConfigOpts client.KubeConfigOptions
	logOptions     logger.Options

	storageAdvAddr string

	availableControllers = []string{
		NodeController.toString(),
		DecommissionController.toString(),
	}
)

//nolint:wsl // the init was generated by kubebuilder
func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(clusterredpandacomv1alpha1.AddToScheme(scheme))
	utilruntime.Must(cmapiv1.AddToScheme(scheme))
	utilruntime.Must(helmControllerAPIv2beta1.AddToScheme(scheme))
	utilruntime.Must(redpandav1alpha1.AddToScheme(scheme))
	utilruntime.Must(sourceControllerAPIv1.AddToScheme(scheme))
	utilruntime.Must(sourceControllerAPIv1beta2.AddToScheme(scheme))
	utilruntime.Must(vectorizedv1alpha1.AddToScheme(scheme))
	//+kubebuilder:scaffold:scheme
}

// +kubebuilder:rbac:groups=coordination.k8s.io,namespace=default,resources=leases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,namespace=default,resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,namespace=default,resources=events,verbs=create;patch

//nolint:funlen,gocyclo // length looks good
func main() {
	var (
		clusterDomain               string
		metricsAddr                 string
		probeAddr                   string
		pprofAddr                   string
		enableLeaderElection        bool
		webhookEnabled              bool
		configuratorBaseImage       string
		configuratorTag             string
		configuratorImagePullPolicy string
		decommissionWaitInterval    time.Duration
		metricsTimeout              time.Duration
		restrictToRedpandaVersion   string
		namespace                   string
		eventsAddr                  string
		additionalControllers       []string
		operatorMode                bool

		// allowPVCDeletion controls the PVC deletion feature in the Cluster custom resource.
		// PVCs will be deleted when its Pod has been deleted and the Node that Pod is assigned to
		// does not exist, or has the NoExecute taint. This is intended to support the rancher.io/local-path
		// storage driver.
		allowPVCDeletion bool
		debug            bool
		ghostbuster      bool
	)

	flag.StringVar(&eventsAddr, "events-addr", "", "The address of the events receiver.")
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.StringVar(&pprofAddr, "pprof-bind-address", ":8082", "The address the metric endpoint binds to.")
	flag.StringVar(&clusterDomain, "cluster-domain", "cluster.local", "Set the Kubernetes local domain (Kubelet's --cluster-domain)")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&webhookEnabled, "webhook-enabled", false, "Enable webhook Manager")
	flag.StringVar(&configuratorBaseImage, "configurator-base-image", defaultConfiguratorContainerImage, "Set the configurator base image")
	flag.StringVar(&configuratorTag, "configurator-tag", "latest", "Set the configurator tag")
	flag.StringVar(&configuratorImagePullPolicy, "configurator-image-pull-policy", "Always", "Set the configurator image pull policy")
	flag.DurationVar(&decommissionWaitInterval, "decommission-wait-interval", 8*time.Second, "Set the time to wait for a node decommission to happen in the cluster")
	flag.DurationVar(&metricsTimeout, "metrics-timeout", 8*time.Second, "Set the timeout for a checking metrics Admin API endpoint. If set to 0, then the 2 seconds default will be used")
	flag.BoolVar(&vectorizedv1alpha1.AllowDownscalingInWebhook, "allow-downscaling", true, "Allow to reduce the number of replicas in existing clusters")
	flag.BoolVar(&allowPVCDeletion, "allow-pvc-deletion", false, "Allow the operator to delete PVCs for Pods assigned to failed or missing Nodes (alpha feature)")
	flag.BoolVar(&vectorizedv1alpha1.AllowConsoleAnyNamespace, "allow-console-any-ns", false, "Allow to create Console in any namespace. Allowing this copies Redpanda SchemaRegistry TLS Secret to namespace (alpha feature)")
	flag.StringVar(&restrictToRedpandaVersion, "restrict-redpanda-version", "", "Restrict management of clusters to those with this version")
	flag.StringVar(&vectorizedv1alpha1.SuperUsersPrefix, "superusers-prefix", "", "Prefix to add in username of superusers managed by operator. This will only affect new clusters, enabling this will not add prefix to existing clusters (alpha feature)")
	flag.BoolVar(&debug, "debug", false, "Set to enable debugging")
	flag.StringVar(&namespace, "namespace", "", "If namespace is set to not empty value, it changes scope of Redpanda operator to work in single namespace")
	flag.BoolVar(&ghostbuster, "unsafe-decommission-failed-brokers", false, "Set to enable decommissioning a failed broker that is configured but does not exist in the StatefulSet (ghost broker). This may result in invalidating valid data")
	_ = flag.CommandLine.MarkHidden("unsafe-decommission-failed-brokers")
	flag.StringSliceVar(&additionalControllers, "additional-controllers", []string{""}, fmt.Sprintf("which controllers to run, available: all, %s", strings.Join(availableControllers, ", ")))
	flag.BoolVar(&operatorMode, "operator-mode", true, "enables to run as an operator, setting this to false will disable cluster (deprecated), redpanda resources reconciliation.")

	logOptions.BindFlags(flag.CommandLine)
	clientOptions.BindFlags(flag.CommandLine)
	kubeConfigOpts.BindFlags(flag.CommandLine)

	flag.Parse()

	ctrl.SetLogger(logger.NewLogger(logOptions))

	if debug {
		go func() {
			pprofMux := http.NewServeMux()
			pprofMux.HandleFunc("/debug/pprof/", pprof.Index)
			pprofMux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
			pprofMux.HandleFunc("/debug/pprof/profile", pprof.Profile)
			pprofMux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
			pprofMux.HandleFunc("/debug/pprof/trace", pprof.Trace)
			pprofServer := &http.Server{
				Addr:              pprofAddr,
				Handler:           pprofMux,
				ReadHeaderTimeout: 3 * time.Second,
			}
			log.Fatal(pprofServer.ListenAndServe())
		}()
	}

	ctx, done := context.WithCancel(context.Background())
	defer done()

	mgrOptions := ctrl.Options{
		Scheme:                  scheme,
		Metrics:                 metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress:  probeAddr,
		LeaderElection:          enableLeaderElection,
		LeaderElectionID:        "aa9fc693.vectorized.io",
		LeaderElectionNamespace: namespace,
	}
	if namespace != "" {
		mgrOptions.Cache.DefaultNamespaces = map[string]cache.Config{namespace: {}}
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), mgrOptions)
	if err != nil {
		setupLog.Error(err, "Unable to start manager")
		// nolint:gocritic // this exits without closing the context. That's ok.
		os.Exit(1)
	}

	configurator := resources.ConfiguratorSettings{
		ConfiguratorBaseImage: configuratorBaseImage,
		ConfiguratorTag:       configuratorTag,
		ImagePullPolicy:       corev1.PullPolicy(configuratorImagePullPolicy),
	}

	// init running state values if we are not in operator mode
	operatorRunningState := ClusterControllerMode
	if namespace != "" {
		operatorRunningState = NamespaceControllerMode
	}

	// but if we are in operator mode, then the run state is different
	if operatorMode {
		operatorRunningState = OperatorV1Mode
		if namespace != "" {
			operatorRunningState = OperatorV2Mode
		}
	}

	// Now we start different processes depending on state
	switch operatorRunningState {
	case OperatorV1Mode:
		ctrl.Log.Info("running in v1", "mode", OperatorV1Mode)

		if err = (&redpandacontrollers.ClusterReconciler{
			Client:                    mgr.GetClient(),
			Log:                       ctrl.Log.WithName("controllers").WithName("redpanda").WithName("Cluster"),
			Scheme:                    mgr.GetScheme(),
			AdminAPIClientFactory:     adminutils.NewInternalAdminAPI,
			DecommissionWaitInterval:  decommissionWaitInterval,
			MetricsTimeout:            metricsTimeout,
			RestrictToRedpandaVersion: restrictToRedpandaVersion,
			GhostDecommissioning:      ghostbuster,
		}).WithClusterDomain(clusterDomain).WithConfiguratorSettings(configurator).WithAllowPVCDeletion(allowPVCDeletion).SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "Unable to create controller", "controller", "Cluster")
			os.Exit(1)
		}

		if err = (&redpandacontrollers.ClusterConfigurationDriftReconciler{
			Client:                    mgr.GetClient(),
			Log:                       ctrl.Log.WithName("controllers").WithName("redpanda").WithName("ClusterConfigurationDrift"),
			Scheme:                    mgr.GetScheme(),
			AdminAPIClientFactory:     adminutils.NewInternalAdminAPI,
			RestrictToRedpandaVersion: restrictToRedpandaVersion,
		}).WithClusterDomain(clusterDomain).SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "Unable to create controller", "controller", "ClusterConfigurationDrift")
			os.Exit(1)
		}

		if err = redpandacontrollers.NewClusterMetricsController(mgr.GetClient()).
			SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "Unable to create controller", "controller", "ClustersMetrics")
			os.Exit(1)
		}

		if err = (&redpandacontrollers.ConsoleReconciler{
			Client:                  mgr.GetClient(),
			Scheme:                  mgr.GetScheme(),
			Log:                     ctrl.Log.WithName("controllers").WithName("redpanda").WithName("Console"),
			AdminAPIClientFactory:   adminutils.NewInternalAdminAPI,
			Store:                   consolepkg.NewStore(mgr.GetClient(), mgr.GetScheme()),
			EventRecorder:           mgr.GetEventRecorderFor("Console"),
			KafkaAdminClientFactory: consolepkg.NewKafkaAdmin,
		}).WithClusterDomain(clusterDomain).SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create controller", "controller", "Console")
			os.Exit(1)
		}

		// Setup webhooks
		if webhookEnabled {
			setupLog.Info("Setup webhook")
			if err = (&vectorizedv1alpha1.Cluster{}).SetupWebhookWithManager(mgr); err != nil {
				setupLog.Error(err, "Unable to create webhook", "webhook", "RedpandaCluster")
				os.Exit(1)
			}
			hookServer := mgr.GetWebhookServer()
			hookServer.Register("/mutate-redpanda-vectorized-io-v1alpha1-console", &webhook.Admission{
				Handler: &redpandawebhooks.ConsoleDefaulter{
					Client:  mgr.GetClient(),
					Decoder: admission.NewDecoder(scheme),
				},
			})
			hookServer.Register("/validate-redpanda-vectorized-io-v1alpha1-console", &webhook.Admission{
				Handler: &redpandawebhooks.ConsoleValidator{
					Client:  mgr.GetClient(),
					Decoder: admission.NewDecoder(scheme),
				},
			})
		}
	case OperatorV2Mode:
		ctrl.Log.Info("running in v2", "mode", OperatorV2Mode, "namespace", namespace)
		storageAddr := ":9090"
		storageAdvAddr = redpandacontrollers.DetermineAdvStorageAddr(storageAddr, setupLog)
		storage := redpandacontrollers.MustInitStorage("/tmp", storageAdvAddr, 60*time.Second, 2, setupLog)

		metricsH := helper.NewMetrics(mgr, metrics.MustMakeRecorder())

		// TODO fill this in with options
		helmOpts := helmController.HelmReleaseReconcilerOptions{
			DependencyRequeueInterval: 30 * time.Second, // The interval at which failing dependencies are reevaluated.
			HTTPRetry:                 9,                // The maximum number of retries when failing to fetch artifacts over HTTP.
			RateLimiter:               workqueue.NewItemExponentialFailureRateLimiter(30*time.Second, 60*time.Second),
		}

		var helmReleaseEventRecorder *events.Recorder
		if helmReleaseEventRecorder, err = events.NewRecorder(mgr, ctrl.Log, eventsAddr, "HelmReleaseReconciler"); err != nil {
			setupLog.Error(err, "unable to create event recorder for: HelmReleaseReconciler")
			os.Exit(1)
		}

		// Helm Release Controller
		helmRelease := helmController.HelmReleaseReconcilerFactory{
			Client:              mgr.GetClient(),
			Config:              mgr.GetConfig(),
			Scheme:              mgr.GetScheme(),
			EventRecorder:       helmReleaseEventRecorder,
			ClientOpts:          clientOptions,
			KubeConfigOpts:      kubeConfigOpts,
			NoCrossNamespaceRef: true,
		}
		if err = helmRelease.SetupWithManager(ctx, mgr, helmOpts); err != nil {
			setupLog.Error(err, "Unable to create controller", "controller", "HelmRelease")
		}

		// Helm Chart Controller
		var helmChartEventRecorder *events.Recorder
		if helmChartEventRecorder, err = events.NewRecorder(mgr, ctrl.Log, eventsAddr, "HelmChartReconciler"); err != nil {
			setupLog.Error(err, "unable to create event recorder for: HelmChartReconciler")
			os.Exit(1)
		}

		chartOpts := helmSourceController.HelmRepositoryReconcilerOptions{}
		helmChart := helmSourceController.HelmChartReconcilerFactory{
			Client:                  mgr.GetClient(),
			RegistryClientGenerator: redpandacontrollers.ClientGenerator,
			Getters:                 getters,
			Metrics:                 metricsH,
			Storage:                 storage,
			EventRecorder:           helmChartEventRecorder,
		}
		if err = helmChart.SetupWithManager(ctx, mgr, chartOpts); err != nil {
			setupLog.Error(err, "Unable to create controller", "controller", "HelmChart")
		}

		// Helm Repository Controller
		var helmRepositoryEventRecorder *events.Recorder
		if helmRepositoryEventRecorder, err = events.NewRecorder(mgr, ctrl.Log, eventsAddr, "HelmRepositoryReconciler"); err != nil {
			setupLog.Error(err, "unable to create event recorder for: HelmRepositoryReconciler")
			os.Exit(1)
		}

		helmRepository := helmSourceController.HelmRepositoryReconcilerFactory{
			Client:         mgr.GetClient(),
			EventRecorder:  helmRepositoryEventRecorder,
			Getters:        getters,
			ControllerName: "redpanda-controller",
			TTL:            15 * time.Minute,
			Metrics:        metricsH,
			Storage:        storage,
		}

		if err = helmRepository.SetupWithManager(ctx, mgr, chartOpts); err != nil {
			setupLog.Error(err, "Unable to create controller", "controller", "HelmRepository")
		}

		go func() {
			// Block until our controller manager is elected leader. We presume our
			// entire process will terminate if we lose leadership, so we don't need
			// to handle that.
			<-mgr.Elected()

			redpandacontrollers.StartFileServer(storage.BasePath, storageAddr, setupLog)
		}()

		// Redpanda Reconciler
		var redpandaEventRecorder *events.Recorder
		if redpandaEventRecorder, err = events.NewRecorder(mgr, ctrl.Log, eventsAddr, "RedpandaReconciler"); err != nil {
			setupLog.Error(err, "unable to create event recorder for: RedpandaReconciler")
			os.Exit(1)
		}

		if err = (&redpandacontrollers.RedpandaReconciler{
			Client:          mgr.GetClient(),
			Scheme:          mgr.GetScheme(),
			EventRecorder:   redpandaEventRecorder,
			RequeueHelmDeps: 10 * time.Second,
		}).SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create controller", "controller", "Redpanda")
			os.Exit(1)
		}

		var topicEventRecorder *events.Recorder
		if topicEventRecorder, err = events.NewRecorder(mgr, ctrl.Log, eventsAddr, "TopicReconciler"); err != nil {
			setupLog.Error(err, "unable to create event recorder for: TopicReconciler")
			os.Exit(1)
		}

		if err = (&clusterredpandacomcontrollers.TopicReconciler{
			Client:        mgr.GetClient(),
			Scheme:        mgr.GetScheme(),
			EventRecorder: topicEventRecorder,
		}).SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create controller", "controller", "Topic")
			os.Exit(1)
		}

		if runThisController(NodeController, additionalControllers) {
			if err = (&redpandacontrollers.RedpandaNodePVCReconciler{
				Client:       mgr.GetClient(),
				OperatorMode: operatorMode,
			}).SetupWithManager(mgr); err != nil {
				setupLog.Error(err, "unable to create controller", "controller", "RedpandaNodePVCReconciler")
				os.Exit(1)
			}
		}

		if runThisController(DecommissionController, additionalControllers) {
			if err = (&redpandacontrollers.DecommissionReconciler{
				Client:       mgr.GetClient(),
				OperatorMode: operatorMode,
			}).SetupWithManager(mgr); err != nil {
				setupLog.Error(err, "unable to create controller", "controller", "DecommissionReconciler")
				os.Exit(1)
			}
		}

	case ClusterControllerMode:
		ctrl.Log.Info("running as a cluster controller", "mode", ClusterControllerMode)
		setupLog.Error(err, "unable to create cluster controllers, not supported")
		os.Exit(1)
	case NamespaceControllerMode:
		ctrl.Log.Info("running as a namespace controller", "mode", NamespaceControllerMode, "namespace", namespace)
		if runThisController(NodeController, additionalControllers) {
			if err = (&redpandacontrollers.RedpandaNodePVCReconciler{
				Client:       mgr.GetClient(),
				OperatorMode: operatorMode,
			}).SetupWithManager(mgr); err != nil {
				setupLog.Error(err, "unable to create controller", "controller", "RedpandaNodePVCReconciler")
				os.Exit(1)
			}
		}

		if runThisController(DecommissionController, additionalControllers) {
			if err = (&redpandacontrollers.DecommissionReconciler{
				Client:       mgr.GetClient(),
				OperatorMode: operatorMode,
			}).SetupWithManager(mgr); err != nil {
				setupLog.Error(err, "unable to create controller", "controller", "DecommissionReconciler")
				os.Exit(1)
			}
		}
	default:
		setupLog.Error(err, "unable unknown state, not supported")
		os.Exit(1)
	}

	//+kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("health", healthz.Ping); err != nil {
		setupLog.Error(err, "Unable to set up health check")
		os.Exit(1)
	}

	if err := mgr.AddReadyzCheck("check", healthz.Ping); err != nil {
		setupLog.Error(err, "Unable to set up ready check")
		os.Exit(1)
	}

	if webhookEnabled {
		hookServer := mgr.GetWebhookServer()
		if err := mgr.AddReadyzCheck("webhook", hookServer.StartedChecker()); err != nil {
			setupLog.Error(err, "unable to create ready check")
			os.Exit(1)
		}

		if err := mgr.AddHealthzCheck("webhook", hookServer.StartedChecker()); err != nil {
			setupLog.Error(err, "unable to create health check")
			os.Exit(1)
		}
	}
	setupLog.Info("Starting manager")

	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "Problem running manager")
		os.Exit(1)
	}
}

func runThisController(rc RedpandaController, controllers []string) bool {
	if len(controllers) == 0 {
		return false
	}

	for _, c := range controllers {
		if RedpandaController(c) == AllControllers || RedpandaController(c) == rc {
			return true
		}
	}
	return false
}
