package cmd

import (
	"context"
	"fmt"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"google.golang.org/grpc/status"

	"github.com/netbirdio/netbird/client/internal"
	"github.com/netbirdio/netbird/client/internal/debug"
	"github.com/netbirdio/netbird/client/internal/peer"
	"github.com/netbirdio/netbird/client/internal/profilemanager"
	"github.com/netbirdio/netbird/client/proto"
	"github.com/netbirdio/netbird/client/server"
	nbstatus "github.com/netbirdio/netbird/client/status"
	mgmProto "github.com/netbirdio/netbird/management/proto"
	"github.com/netbirdio/netbird/upload-server/types"
)

const errCloseConnection = "Failed to close connection: %v"

var (
	logFileCount        uint32
	systemInfoFlag      bool
	uploadBundleFlag    bool
	uploadBundleURLFlag string
)

var debugCmd = &cobra.Command{
	Use:   "debug",
	Short: "Debugging commands",
	Long:  "Provides commands for debugging and logging control within the Netbird daemon.",
}

var debugBundleCmd = &cobra.Command{
	Use:     "bundle",
	Example: "  netbird debug bundle",
	Short:   "Create a debug bundle",
	Long:    "Generates a compressed archive of the daemon's logs and status for debugging purposes.",
	RunE:    debugBundle,
}

var logCmd = &cobra.Command{
	Use:   "log",
	Short: "Manage logging for the Netbird daemon",
	Long:  `Commands to manage logging settings for the Netbird daemon, including ICE, gRPC, and general log levels.`,
}

var logLevelCmd = &cobra.Command{
	Use:   "level <level>",
	Short: "Set the logging level for this session",
	Long: `Sets the logging level for the current session. This setting is temporary and will revert to the default on daemon restart.
Available log levels are:
  panic:   for panic level, highest level of severity
  fatal:   for fatal level errors that cause the program to exit
  error:   for error conditions
  warn:    for warning conditions
  info:    for informational messages
  debug:   for debug-level messages
  trace:   for trace-level messages, which include more fine-grained information than debug`,
	Args: cobra.ExactArgs(1),
	RunE: setLogLevel,
}

var forCmd = &cobra.Command{
	Use:     "for <time>",
	Short:   "Run debug logs for a specified duration and create a debug bundle",
	Long:    `Sets the logging level to trace, runs for the specified duration, and then generates a debug bundle.`,
	Example: "  netbird debug for 5m",
	Args:    cobra.ExactArgs(1),
	RunE:    runForDuration,
}

var persistenceCmd = &cobra.Command{
	Use:     "persistence [on|off]",
	Short:   "Set network map memory persistence",
	Long:    `Configure whether the latest network map should persist in memory. When enabled, the last known network map will be kept in memory.`,
	Example: "  netbird debug persistence on",
	Args:    cobra.ExactArgs(1),
	RunE:    setNetworkMapPersistence,
}

func debugBundle(cmd *cobra.Command, _ []string) error {
	conn, err := getClient(cmd)
	if err != nil {
		return err
	}
	defer func() {
		if err := conn.Close(); err != nil {
			log.Errorf(errCloseConnection, err)
		}
	}()

	client := proto.NewDaemonServiceClient(conn)
	request := &proto.DebugBundleRequest{
		Anonymize:    anonymizeFlag,
		Status:       getStatusOutput(cmd, anonymizeFlag),
		SystemInfo:   systemInfoFlag,
		LogFileCount: logFileCount,
	}
	if uploadBundleFlag {
		request.UploadURL = uploadBundleURLFlag
	}
	resp, err := client.DebugBundle(cmd.Context(), request)
	if err != nil {
		return fmt.Errorf("failed to bundle debug: %v", status.Convert(err).Message())
	}
	cmd.Printf("Local file:\n%s\n", resp.GetPath())

	if resp.GetUploadFailureReason() != "" {
		return fmt.Errorf("upload failed: %s", resp.GetUploadFailureReason())
	}

	if uploadBundleFlag {
		cmd.Printf("Upload file key:\n%s\n", resp.GetUploadedKey())
	}

	return nil
}

func setLogLevel(cmd *cobra.Command, args []string) error {
	conn, err := getClient(cmd)
	if err != nil {
		return err
	}
	defer func() {
		if err := conn.Close(); err != nil {
			log.Errorf(errCloseConnection, err)
		}
	}()

	client := proto.NewDaemonServiceClient(conn)
	level := server.ParseLogLevel(args[0])
	if level == proto.LogLevel_UNKNOWN {
		return fmt.Errorf("unknown log level: %s. Available levels are: panic, fatal, error, warn, info, debug, trace\n", args[0])
	}

	_, err = client.SetLogLevel(cmd.Context(), &proto.SetLogLevelRequest{
		Level: level,
	})
	if err != nil {
		return fmt.Errorf("failed to set log level: %v", status.Convert(err).Message())
	}

	cmd.Println("Log level set successfully to", args[0])
	return nil
}

func runForDuration(cmd *cobra.Command, args []string) error {
	duration, err := time.ParseDuration(args[0])
	if err != nil {
		return fmt.Errorf("invalid duration format: %v", err)
	}

	conn, err := getClient(cmd)
	if err != nil {
		return err
	}
	defer func() {
		if err := conn.Close(); err != nil {
			log.Errorf(errCloseConnection, err)
		}
	}()

	client := proto.NewDaemonServiceClient(conn)

	stat, err := client.Status(cmd.Context(), &proto.StatusRequest{})
	if err != nil {
		return fmt.Errorf("failed to get status: %v", status.Convert(err).Message())
	}

	stateWasDown := stat.Status != string(internal.StatusConnected) && stat.Status != string(internal.StatusConnecting)

	initialLogLevel, err := client.GetLogLevel(cmd.Context(), &proto.GetLogLevelRequest{})
	if err != nil {
		return fmt.Errorf("failed to get log level: %v", status.Convert(err).Message())
	}

	if stateWasDown {
		if _, err := client.Up(cmd.Context(), &proto.UpRequest{}); err != nil {
			return fmt.Errorf("failed to up: %v", status.Convert(err).Message())
		}
		cmd.Println("Netbird up")
		time.Sleep(time.Second * 10)
	}

	initialLevelTrace := initialLogLevel.GetLevel() >= proto.LogLevel_TRACE
	if !initialLevelTrace {
		_, err = client.SetLogLevel(cmd.Context(), &proto.SetLogLevelRequest{
			Level: proto.LogLevel_TRACE,
		})
		if err != nil {
			return fmt.Errorf("failed to set log level to TRACE: %v", status.Convert(err).Message())
		}
		cmd.Println("Log level set to trace.")
	}

	if _, err := client.Down(cmd.Context(), &proto.DownRequest{}); err != nil {
		return fmt.Errorf("failed to down: %v", status.Convert(err).Message())
	}
	cmd.Println("Netbird down")

	time.Sleep(1 * time.Second)

	// Enable network map persistence before bringing the service up
	if _, err := client.SetNetworkMapPersistence(cmd.Context(), &proto.SetNetworkMapPersistenceRequest{
		Enabled: true,
	}); err != nil {
		return fmt.Errorf("failed to enable network map persistence: %v", status.Convert(err).Message())
	}

	if _, err := client.Up(cmd.Context(), &proto.UpRequest{}); err != nil {
		return fmt.Errorf("failed to up: %v", status.Convert(err).Message())
	}
	cmd.Println("Netbird up")

	time.Sleep(3 * time.Second)

	headerPostUp := fmt.Sprintf("----- Netbird post-up - Timestamp: %s", time.Now().Format(time.RFC3339))
	statusOutput := fmt.Sprintf("%s\n%s", headerPostUp, getStatusOutput(cmd, anonymizeFlag))

	if waitErr := waitForDurationOrCancel(cmd.Context(), duration, cmd); waitErr != nil {
		return waitErr
	}
	cmd.Println("\nDuration completed")

	cmd.Println("Creating debug bundle...")

	headerPreDown := fmt.Sprintf("----- Netbird pre-down - Timestamp: %s - Duration: %s", time.Now().Format(time.RFC3339), duration)
	statusOutput = fmt.Sprintf("%s\n%s\n%s", statusOutput, headerPreDown, getStatusOutput(cmd, anonymizeFlag))
	request := &proto.DebugBundleRequest{
		Anonymize:    anonymizeFlag,
		Status:       statusOutput,
		SystemInfo:   systemInfoFlag,
		LogFileCount: logFileCount,
	}
	if uploadBundleFlag {
		request.UploadURL = uploadBundleURLFlag
	}
	resp, err := client.DebugBundle(cmd.Context(), request)
	if err != nil {
		return fmt.Errorf("failed to bundle debug: %v", status.Convert(err).Message())
	}

	if stateWasDown {
		if _, err := client.Down(cmd.Context(), &proto.DownRequest{}); err != nil {
			return fmt.Errorf("failed to down: %v", status.Convert(err).Message())
		}
		cmd.Println("Netbird down")
	}

	if !initialLevelTrace {
		if _, err := client.SetLogLevel(cmd.Context(), &proto.SetLogLevelRequest{Level: initialLogLevel.GetLevel()}); err != nil {
			return fmt.Errorf("failed to restore log level: %v", status.Convert(err).Message())
		}
		cmd.Println("Log level restored to", initialLogLevel.GetLevel())
	}

	cmd.Printf("Local file:\n%s\n", resp.GetPath())

	if resp.GetUploadFailureReason() != "" {
		return fmt.Errorf("upload failed: %s", resp.GetUploadFailureReason())
	}

	if uploadBundleFlag {
		cmd.Printf("Upload file key:\n%s\n", resp.GetUploadedKey())
	}

	return nil
}

func setNetworkMapPersistence(cmd *cobra.Command, args []string) error {
	conn, err := getClient(cmd)
	if err != nil {
		return err
	}
	defer func() {
		if err := conn.Close(); err != nil {
			log.Errorf(errCloseConnection, err)
		}
	}()

	persistence := strings.ToLower(args[0])
	if persistence != "on" && persistence != "off" {
		return fmt.Errorf("invalid persistence value: %s. Use 'on' or 'off'", args[0])
	}

	client := proto.NewDaemonServiceClient(conn)
	_, err = client.SetNetworkMapPersistence(cmd.Context(), &proto.SetNetworkMapPersistenceRequest{
		Enabled: persistence == "on",
	})
	if err != nil {
		return fmt.Errorf("failed to set network map persistence: %v", status.Convert(err).Message())
	}

	cmd.Printf("Network map persistence set to: %s\n", persistence)
	return nil
}

func getStatusOutput(cmd *cobra.Command, anon bool) string {
	var statusOutputString string
	statusResp, err := getStatus(cmd.Context())
	if err != nil {
		cmd.PrintErrf("Failed to get status: %v\n", err)
	} else {
		statusOutputString = nbstatus.ParseToFullDetailSummary(
			nbstatus.ConvertToStatusOutputOverview(statusResp, anon, "", nil, nil, nil, "", ""),
		)
	}
	return statusOutputString
}

func waitForDurationOrCancel(ctx context.Context, duration time.Duration, cmd *cobra.Command) error {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	startTime := time.Now()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				elapsed := time.Since(startTime)
				if elapsed >= duration {
					return
				}
				remaining := duration - elapsed
				cmd.Printf("\rRemaining time: %s", formatDuration(remaining))
			}
		}
	}()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-done:
		return nil
	}
}

func formatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	h := d / time.Hour
	d %= time.Hour
	m := d / time.Minute
	d %= time.Minute
	s := d / time.Second
	return fmt.Sprintf("%02d:%02d:%02d", h, m, s)
}

func generateDebugBundle(config *profilemanager.Config, recorder *peer.Status, connectClient *internal.ConnectClient, logFilePath string) {
	var networkMap *mgmProto.NetworkMap
	var err error

	if connectClient != nil {
		networkMap, err = connectClient.GetLatestNetworkMap()
		if err != nil {
			log.Warnf("Failed to get latest network map: %v", err)
		}
	}

	bundleGenerator := debug.NewBundleGenerator(
		debug.GeneratorDependencies{
			InternalConfig: config,
			StatusRecorder: recorder,
			NetworkMap:     networkMap,
			LogFile:        logFilePath,
		},
		debug.BundleConfig{
			IncludeSystemInfo: true,
		},
	)

	path, err := bundleGenerator.Generate()
	if err != nil {
		log.Errorf("Failed to generate debug bundle: %v", err)
		return
	}
	log.Infof("Generated debug bundle from SIGUSR1 at: %s", path)
}

func init() {
	debugBundleCmd.Flags().Uint32VarP(&logFileCount, "log-file-count", "C", 1, "Number of rotated log files to include in debug bundle")
	debugBundleCmd.Flags().BoolVarP(&systemInfoFlag, "system-info", "S", true, "Adds system information to the debug bundle")
	debugBundleCmd.Flags().BoolVarP(&uploadBundleFlag, "upload-bundle", "U", false, "Uploads the debug bundle to a server")
	debugBundleCmd.Flags().StringVar(&uploadBundleURLFlag, "upload-bundle-url", types.DefaultBundleURL, "Service URL to get an URL to upload the debug bundle")

	forCmd.Flags().Uint32VarP(&logFileCount, "log-file-count", "C", 1, "Number of rotated log files to include in debug bundle")
	forCmd.Flags().BoolVarP(&systemInfoFlag, "system-info", "S", true, "Adds system information to the debug bundle")
	forCmd.Flags().BoolVarP(&uploadBundleFlag, "upload-bundle", "U", false, "Uploads the debug bundle to a server")
	forCmd.Flags().StringVar(&uploadBundleURLFlag, "upload-bundle-url", types.DefaultBundleURL, "Service URL to get an URL to upload the debug bundle")
}
