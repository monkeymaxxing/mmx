// The program of the test image that the container suites pull from the test
// registry (tests/integration/image_registry.py). It is the only file of the
// image, /container-test-program, so it uses the standard library only and is
// built without cgo.
//
// As the container's command it serves HTTP on port 8080, or on PORT:
//
//	GET /        JSON: image_version (the image's IMAGE_VERSION),
//	             process_started_at, configuration (the text of
//	             /etc/container-test-program/configuration as read at the
//	             start and at each SIGHUP; empty when there is no file), and
//	             configuration_reloads (the number of SIGHUPs).
//	GET /health  200, or 503 while the file /data/unhealthy exists.
//
// At its start it appends one line to /data/starts.log when the directory
// /data exists. With EXIT_AT_START_WITH_CODE=<number> it exits at once with
// that code instead, before it writes or serves anything. On SIGTERM it
// exits at once with code 0.
//
// Two commands are for use inside a running container:
//
//	/container-test-program check-health
//	    exits 0 when GET /health answers 200 and 1 otherwise.
//	/container-test-program reload-configuration
//	    sends SIGHUP to process 1 of the container and returns when that
//	    process has read its configuration again.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"
)

const (
	configurationFile = "/etc/container-test-program/configuration"
	dataDirectory     = "/data"
	startsLog         = dataDirectory + "/starts.log"
	unhealthyFile     = dataDirectory + "/unhealthy"
)

// answer is the JSON document of GET /.
type answer struct {
	ImageVersion         string `json:"image_version"`
	ProcessStartedAt     string `json:"process_started_at"`
	Configuration        string `json:"configuration"`
	ConfigurationReloads int    `json:"configuration_reloads"`
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	if len(os.Args) == 1 {
		serve(port)
	}
	ownAddress := "http://127.0.0.1:" + port
	switch os.Args[1] {
	case "check-health":
		checkHealth(ownAddress)
	case "reload-configuration":
		reloadConfiguration(ownAddress)
	default:
		fail(2, "unknown command %q; the commands are check-health and reload-configuration", os.Args[1])
	}
}

func fail(code int, format string, values ...any) {
	fmt.Fprintf(os.Stderr, "container-test-program: "+format+"\n", values...)
	os.Exit(code)
}

func serve(port string) {
	if text := os.Getenv("EXIT_AT_START_WITH_CODE"); text != "" {
		code, err := strconv.Atoi(text)
		if err != nil {
			fail(2, "EXIT_AT_START_WITH_CODE is %q, not a number", text)
		}
		os.Exit(code)
	}
	// Process 1 of a container gets only the signals it asks for, so the
	// program asks before anything else.
	signals := make(chan os.Signal, 8)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGHUP)

	var lock sync.Mutex
	current := answer{
		ImageVersion:     os.Getenv("IMAGE_VERSION"),
		ProcessStartedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Configuration:    readConfiguration(),
	}
	if directory, err := os.Stat(dataDirectory); err == nil && directory.IsDir() {
		line := current.ProcessStartedAt + " image_version=" + current.ImageVersion + "\n"
		log, err := os.OpenFile(startsLog, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err == nil {
			_, err = log.WriteString(line)
		}
		if err != nil {
			fail(1, "%v", err)
		}
		log.Close()
	}
	go func() {
		for received := range signals {
			if received == syscall.SIGTERM {
				os.Exit(0)
			}
			text := readConfiguration()
			lock.Lock()
			current.Configuration = text
			current.ConfigurationReloads++
			lock.Unlock()
		}
	}()

	http.HandleFunc("/", func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/" {
			http.NotFound(response, request)
			return
		}
		lock.Lock()
		document, _ := json.Marshal(current)
		lock.Unlock()
		response.Header().Set("Content-Type", "application/json")
		// The client detects a failed response write when reading the HTTP response.
		_, _ = response.Write(append(document, '\n'))
	})
	http.HandleFunc("/health", func(response http.ResponseWriter, request *http.Request) {
		if _, err := os.Stat(unhealthyFile); err == nil {
			http.Error(response, "unhealthy: "+unhealthyFile+" exists", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(response, "healthy")
	})
	fail(1, "%v", http.ListenAndServe(":"+port, nil))
}

// readConfiguration returns the configuration file's text, or the empty
// string when the file cannot be read.
func readConfiguration() string {
	text, err := os.ReadFile(configurationFile)
	if err != nil {
		return ""
	}
	return string(text)
}

var client = http.Client{Timeout: 2 * time.Second}

func checkHealth(ownAddress string) {
	response, err := client.Get(ownAddress + "/health")
	if err != nil {
		fail(1, "%v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		fail(1, "GET /health answered %s", response.Status)
	}
	os.Exit(0)
}

func configurationReloads(ownAddress string) (int, error) {
	response, err := client.Get(ownAddress + "/")
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	var current answer
	if err := json.NewDecoder(response.Body).Decode(&current); err != nil {
		return 0, err
	}
	return current.ConfigurationReloads, nil
}

// reloadConfiguration returns only when process 1 has counted the reload, so
// that a test can read the new configuration right after this command.
func reloadConfiguration(ownAddress string) {
	before, err := configurationReloads(ownAddress)
	if err != nil {
		fail(1, "process 1 of this container does not answer: %v", err)
	}
	if err := syscall.Kill(1, syscall.SIGHUP); err != nil {
		fail(1, "sending SIGHUP to process 1: %v", err)
	}
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if now, err := configurationReloads(ownAddress); err == nil && now > before {
			os.Exit(0)
		}
	}
	fail(1, "process 1 did not read its configuration again within 5 seconds")
}
