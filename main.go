package main

import (
	"flag"
	"net/http"
	"os"
	"time"

	"go.uber.org/zap/zapcore"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	organizationsv1alpha1 "github.com/fcp/aws-account-controller/api/v1alpha1"
	"github.com/fcp/aws-account-controller/controllers"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(organizationsv1alpha1.AddToScheme(scheme))
}

func main() {
	var metricsAddr string
	var enableLeaderElection bool
	var probeAddr string
	var logLevel string

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false, "Enable leader election for controller manager.")
	flag.StringVar(&logLevel, "log-level", "info", "Log level (debug, info, warn, error)")

	opts := zap.Options{
		Development: true,
	}

	switch logLevel {
	case "debug":
		opts.Level = zapcore.DebugLevel
	case "info":
		opts.Level = zapcore.InfoLevel
	case "warn":
		opts.Level = zapcore.WarnLevel
	case "error":
		opts.Level = zapcore.ErrorLevel
	default:
		opts.Level = zapcore.InfoLevel
	}

	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// Print startup information
	setupLog.Info("Starting AWS Account Controller",
		"version", getVersion(),
		"logLevel", logLevel,
		"metricsAddr", metricsAddr,
		"probeAddr", probeAddr,
		"leaderElection", enableLeaderElection)

	// Create manager
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: metricsAddr,
		},
		HealthProbeBindAddress:  probeAddr,
		LeaderElection:          enableLeaderElection,
		LeaderElectionID:        "aws-account-controller-leader",
		GracefulShutdownTimeout: &[]time.Duration{30 * time.Second}[0],
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// Setup Account Controller
	setupLog.Info("Setting up Account controller")
	accountReconciler := controllers.NewAccountReconciler(mgr.GetClient(), mgr.GetScheme())
	if err = accountReconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Account")
		os.Exit(1)
	}

	// Setup AdoptedAccount Controller
	setupLog.Info("Setting up AdoptedAccount controller")
	if err = (&controllers.AdoptedAccountReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "AdoptedAccount")
		os.Exit(1)
	}

	// Add health checks
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", func(req *http.Request) error {
		return healthz.Ping(req)
	}); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	// Print environment configuration
	printEnvironmentInfo()

	// Start the manager
	setupLog.Info("Starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

// getVersion returns the version information
func getVersion() string {
	version := os.Getenv("CONTROLLER_VERSION")
	if version == "" {
		version = "dev"
	}
	return version
}

// printEnvironmentInfo logs relevant environment variables for debugging
func printEnvironmentInfo() {
	setupLog.Info("Environment configuration",
		"AWS_ACCOUNT_ID", getEnvOrDefault("AWS_ACCOUNT_ID", "not set"),
		"ORG_MGMT_ACCOUNT_ID", getEnvOrDefault("ORG_MGMT_ACCOUNT_ID", "not set"),
		"DEFAULT_ORGANIZATIONAL_UNIT_ID", getEnvOrDefault("DEFAULT_ORGANIZATIONAL_UNIT_ID", "not set"),
		"ACCOUNT_DELETION_MODE", getEnvOrDefault("ACCOUNT_DELETION_MODE", "soft (default)"),
		"DELETED_ACCOUNTS_OU_ID", getEnvOrDefault("DELETED_ACCOUNTS_OU_ID", "not set"),
	)

	required := []string{
		"AWS_ACCOUNT_ID",
		"ORG_MGMT_ACCOUNT_ID",
	}

	for _, env := range required {
		if os.Getenv(env) == "" {
			setupLog.Info("Warning: Required environment variable not set", "variable", env)
		}
	}

	if os.Getenv("AWS_ACCESS_KEY_ID") == "" && os.Getenv("AWS_PROFILE") == "" {
		setupLog.Info("Warning: No AWS credentials found in environment. Ensure the controller has proper AWS credentials via IAM roles, instance profile, or environment variables")
	}
}

// getEnvOrDefault returns environment variable value or default
func getEnvOrDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}
