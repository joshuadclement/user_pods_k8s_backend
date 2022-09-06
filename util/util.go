package util

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"path"
	"regexp"
	"sync"
	"time"

	yaml "gopkg.in/yaml.v3"

	apiv1 "k8s.io/api/core/v1"
)

const configFile = "config.yaml"

// type for signalling whether one-off events have completed successfully within a timeout
type ReadyChannel struct {
	ch          chan bool
	receivedYet bool
	firstValue  bool
	mutex       *sync.Mutex
}

// Return a new safeBoolChannel whith the timeout counting down
func NewReadyChannel(timeout time.Duration) *ReadyChannel {
	ch := make(chan bool, 1)
	var m sync.Mutex
	rc := &ReadyChannel{
		ch:          ch,
		receivedYet: false,
		firstValue:  false,
		mutex:       &m,
	}
	go func() {
		time.Sleep(timeout)
		rc.Send(false)
	}()
	return rc
}

// Attempt to send value into the ReadyChannel's channel.
// If the buffer is already full, this will do nothing.
func (t *ReadyChannel) Send(value bool) {
	select {
	case t.ch <- value:
	default:
	}
}

// Return the first value that was input to t.Send().
// If there hasn't been one yet, block until there is one.
func (t *ReadyChannel) Receive() bool {
	// use the ReadyChannel's mutex to block other goroutines where t.Receive is called until this returns
	t.mutex.Lock()
	defer func() {
		t.mutex.Unlock()
	}()

	// if a value has been received from this ReadyChannel, return that value
	if t.receivedYet {
		return t.firstValue
	}
	// otherwise, this is the first time Receive is called
	// block until the first value is ready in the channel, which will either be from t.Send() or the timeout
	value := <-t.ch

	// set t.firstValue to true so that subsequent t.Receive() will return value immediately
	t.receivedYet = true
	t.firstValue = value
	return value
}

// Block until an input was received from each channel in inputChannels,
// then send output <- input0 && input 1 && input2...
func CombineReadyChannels(inputChannels []*ReadyChannel, outputChannel *ReadyChannel) {
	output := ReceiveReadyChannels(inputChannels)
	outputChannel.Send(output)
}

func ReceiveReadyChannels(inputChannels []*ReadyChannel) bool {
	output := true
	for _, ch := range inputChannels {
		// Block until able to receive from each channel,
		// if any are false, then the output is false
		if !ch.Receive() {
			output = false
		}
	}
	return output
}

// Gets the IP of the source that made the request, either r.RemoteAddr,
// or if it was forwarded, the first address in the X-Forwarded-For header
func GetRemoteIP(r *http.Request) string {
	// When running this behind caddy, r.RemoteAddr is just the caddy process's IP addr,
	// and X-Forward-For header should contain the silo's IP address.
	// This may be different with ingress.
	var siloIP string
	value, forwarded := r.Header["X-Forwarded-For"]
	if forwarded {
		siloIP = value[0]
	} else {
		siloIP = r.RemoteAddr
	}
	return regexp.MustCompile(`(\d{1,3}[.]){3}\d{1,3}`).FindString(siloIP)
}

func GetUserIDFromLabels(labels map[string]string) string {
	user, hasUser := labels["user"]
	if !hasUser {
		return ""
	}
	if user == "" {
		return ""
	}
	domain, hasDomain := labels["domain"]
	if hasDomain {
		if domain != "" {
			return fmt.Sprintf("%s@%s", user, domain)
		}
	}
	return user
}

type OptionalRestartPolicy string

const (
	RestartPolicyAlways    OptionalRestartPolicy = OptionalRestartPolicy(apiv1.RestartPolicyAlways)
	RestartPolicyOnFailure OptionalRestartPolicy = OptionalRestartPolicy(apiv1.RestartPolicyOnFailure)
	RestartPolicyNever     OptionalRestartPolicy = OptionalRestartPolicy(apiv1.RestartPolicyNever)
	RestartPolicyDefault   OptionalRestartPolicy = "Default"
)

type GlobalConfig struct {
	RestartPolicy          apiv1.RestartPolicy
	TimeoutCreate          time.Duration
	TimeoutDelete          time.Duration
	Namespace              string
	TokenDir               string
	PublicIP               string
	WhitelistManifestRegex string
	TokenByteLimit         int
	NfsStorageRoot         string
	MandatoryEnvVars       map[string]string
}

func getConfigFilename() string {
	goPath := os.Getenv("GOPATH")
	return path.Join(goPath, "src/user_pods_k8s_backend/config.yaml")
}

func SaveGlobalConfig(c GlobalConfig) error {
	buffer := new(bytes.Buffer)
	encoder := yaml.NewEncoder(buffer)
	err := encoder.Encode(c)
	if err != nil {
		return err
	}
	err = ioutil.WriteFile(getConfigFilename(), buffer.Bytes(), 0600)
	if err != nil {
		return err
	}
	return nil
}

func MustLoadGlobalConfig() GlobalConfig {
	var config GlobalConfig
	// Open the config file
	file, err := os.Open(getConfigFilename())
	if err != nil {
		panic(err.Error())
	}
	// Decode into the config struct
	decoder := yaml.NewDecoder(file)
	err = decoder.Decode(&config)
	if err != nil {
		panic(err.Error())
	}

	// Check that WhitelistManifestRegex compiles to a regex
	_, err = regexp.Compile(config.WhitelistManifestRegex)
	if err != nil {
		panic(fmt.Sprintf("Invalid WhitelistManifestRegex in config: %s", err.Error()))
	}

	// Check that RestartPolicy is an allowed value
	switch config.RestartPolicy {
	case apiv1.RestartPolicyAlways:
	case apiv1.RestartPolicyOnFailure:
	case apiv1.RestartPolicyNever:
	case "":
	default:
		panic(fmt.Sprintf("Invalid restart policy. Must be \"Always\", \"OnFailure\", \"Never\", or empty"))
	}

	// Check that PublicIP is an IP address (currently only v4)
	ipRegex := regexp.MustCompile(`^(\d{1,3}[.]){3}\d{1,3}$`)
	if !ipRegex.MatchString(config.PublicIP) {
		panic(fmt.Sprintf("Public IP %s not a valid ip address", config.PublicIP))
	}

	return config
}
