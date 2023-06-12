/*


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
	"context"
	"flag"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/contrib/propagators/b3"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/sdk/trace"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	infradoodlecomv1beta1 "github.com/DoodleScheduling/k8sreq-duplicator-controller/api/v1beta1"
	"github.com/DoodleScheduling/k8sreq-duplicator-controller/controllers"
	"github.com/DoodleScheduling/k8sreq-duplicator-controller/proxy"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	_ = clientgoscheme.AddToScheme(scheme)

	_ = corev1.AddToScheme(scheme)
	_ = infradoodlecomv1beta1.AddToScheme(scheme)
	// +kubebuilder:scaffold:scheme
}

var (
	proxyReadTimeout        = "30s"
	proxyWriteTimeout       = "30s"
	httpAddr                = ":8080"
	metricsAddr             = ":9556"
	probesAddr              = ":9557"
	enableLeaderElection    = false
	leaderElectionNamespace string
	namespaces              = ""
	concurrent              = 2
)

func main() {
	flag.StringVar(&metricsAddr, "metrics-addr", ":9556", "The address of the metric endpoint binds to.")
	flag.StringVar(&probesAddr, "probe-addr", ":9557", "The address of the probe endpoints bind to.")
	flag.StringVar(&httpAddr, "http-addr", ":8080", "The address of http server binding to.")
	flag.StringVar(&proxyReadTimeout, "proxy-read-timeout", "10s", "Read timeout for proxy requests.")
	flag.StringVar(&proxyWriteTimeout, "proxy-write-timeout", "10s", "Write timeout for proxy requests.")
	flag.BoolVar(&enableLeaderElection, "enable-leader-election", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.StringVar(&leaderElectionNamespace, "leader-election-namespace", "",
		"Specify a different leader election namespace. It will use the one where the controller is deployed by default.")
	flag.StringVar(&namespaces, "namespaces", "",
		"The controller listens by default for all namespaces. This may be limited to a comma delimted list of dedicated namespaces.")
	flag.IntVar(&concurrent, "concurrent", 2,
		"The number of concurrent reconcile workers. By default this is 2.")

	pflag.CommandLine.AddGoFlagSet(flag.CommandLine)
	pflag.Parse()
	ctrl.SetLogger(zap.New())

	// Import flags into viper and bind them to env vars
	// flags are converted to upper-case, - is replaced with _
	err := viper.BindPFlags(pflag.CommandLine)
	if err != nil {
		setupLog.Error(err, "Failed parsing command line arguments")
		os.Exit(1)
	}

	replacer := strings.NewReplacer("-", "_")
	viper.SetEnvKeyReplacer(replacer)
	viper.AutomaticEnv()

	opts := ctrl.Options{
		Scheme:                  scheme,
		MetricsBindAddress:      viper.GetString("metrics-addr"),
		HealthProbeBindAddress:  viper.GetString("probe-addr"),
		LeaderElection:          viper.GetBool("enable-leader-election"),
		LeaderElectionNamespace: viper.GetString("leader-election-namespace"),
		LeaderElectionID:        "k8sreq-duplicator-controller",
	}

	ns := strings.Split(viper.GetString("namespaces"), ",")
	if len(ns) > 0 && ns[0] != "" {
		opts.NewCache = cache.MultiNamespacedCacheBuilder(ns)
		setupLog.Info("watching dedicated namespaces", "namespaces", ns)
	} else {
		setupLog.Info("watching all namespaces")
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), opts)
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// Add liveness probe
	err = mgr.AddHealthzCheck("healthz", healthz.Ping)
	if err != nil {
		setupLog.Error(err, "Could not add liveness probe")
		os.Exit(1)
	}

	// Add readiness probe
	err = mgr.AddReadyzCheck("readyz", healthz.Ping)
	if err != nil {
		setupLog.Error(err, "Could not add readiness probe")
		os.Exit(1)
	}

	resources, err := resource.New(context.Background(),
		resource.WithFromEnv(), // pull attributes from OTEL_RESOURCE_ATTRIBUTES and OTEL_SERVICE_NAME environment variables
		resource.WithProcess(), // This option configures a set of Detectors that discover process information
	)
	if err != nil {
		setupLog.Error(err, "failed creating OTLP resources")
		os.Exit(1)
	}

	client := otlptracegrpc.NewClient()
	exporter, err := otlptrace.New(context.Background(), client)
	if err != nil {
		setupLog.Error(err, "failed creating OTLP trace exporter")
		os.Exit(1)
	}

	tp := trace.NewTracerProvider(
		trace.WithBatcher(exporter),
		trace.WithResource(resources),
	)

	otel.SetTextMapPropagator(b3.New())
	defer func() {
		if err := tp.Shutdown(context.Background()); err != nil {
			setupLog.Error(err, "failed to shutdown trace provider")
		}
	}()

	otel.SetTracerProvider(tp)
	o := proxy.Options{
		Logger: setupLog,
		Client: &http.Client{
			Transport: otelhttp.NewTransport(http.DefaultTransport),
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		BodySizeLimit: proxy.DefaultBodyLimit,
	}

	proxy := proxy.New(o)
	wrappedHandler := otelhttp.NewHandler(proxy, "req-duplicator")

	s := &http.Server{
		Addr:           httpAddr,
		Handler:        wrappedHandler,
		ReadTimeout:    10 * time.Second,
		WriteTimeout:   10 * time.Second,
		MaxHeaderBytes: 1 << 20,
	}

	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		defer wg.Done()
		if err := s.ListenAndServe(); err != nil {
			setupLog.Error(err, "HTTP server error")
		}

		s.RegisterOnShutdown(func() {
			proxy.Close()
		})
	}()

	pReconciler := &controllers.RequestCloneReconciler{
		Client:    mgr.GetClient(),
		Log:       ctrl.Log.WithName("controllers").WithName("RequestClone"),
		Scheme:    mgr.GetScheme(),
		Recorder:  mgr.GetEventRecorderFor("RequestClone"),
		HttpProxy: proxy,
	}
	if err = pReconciler.SetupWithManager(mgr, controllers.RequestCloneReconcilerOptions{MaxConcurrentReconciles: viper.GetInt("concurrent")}); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "RequestClone")
		os.Exit(1)
	}

	// +kubebuilder:scaffold:builder

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}

	_ = s.Shutdown(context.TODO())
	wg.Wait()
}
