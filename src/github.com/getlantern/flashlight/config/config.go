package config

import (
	"compress/gzip"
	"encoding/hex"
	"fmt"
	"io/ioutil"
	"math/rand"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"sync/atomic"
	"time"

	"code.google.com/p/go-uuid/uuid"
	"github.com/go-fsnotify/fsnotify"
	"github.com/spf13/viper"

	"github.com/getlantern/appdir"
	"github.com/getlantern/fronted"
	"github.com/getlantern/golog"
	"github.com/getlantern/proxiedsites"
	"github.com/getlantern/yaml"

	"github.com/getlantern/flashlight/client"
	"github.com/getlantern/flashlight/globals"
	"github.com/getlantern/flashlight/server"
	"github.com/getlantern/flashlight/statreporter"
)

const (
	CloudConfigPollInterval = 1 * time.Minute
	cloudflare              = "cloudflare"
	etag                    = "X-Lantern-Etag"
	ifNoneMatch             = "X-Lantern-If-None-Match"
)

var (
	log                 = golog.LoggerFor("flashlight.config")
	lastCloudConfigETag = map[string]string{}
	httpClient          atomic.Value
)

type Config struct {
	Version       int
	CloudConfig   string
	CloudConfigCA string
	Addr          string
	Role          string
	InstanceId    string
	CpuProfile    string
	MemProfile    string
	UIAddr        string // UI HTTP server address
	AutoReport    *bool  // Report anonymous usage to GA
	AutoLaunch    *bool  // Automatically launch Lantern on system startup
	Stats         *statreporter.Config
	Server        *server.ServerConfig
	Client        *client.ClientConfig
	ProxiedSites  *proxiedsites.Config // List of proxied site domains that get routed through Lantern rather than accessed directly
	TrustedCAs    []*CA
}

func Configure(c *http.Client) {
	httpClient.Store(c)
	// No-op if already started.
	// TODO: fs-notify
	//m.StartPolling()
}

// CA represents a certificate authority
type CA struct {
	CommonName string
	Cert       string // PEM-encoded
}

// Init initializes the configuration system.
func Init() (*Config, error) {
	configPath, err := InConfigDir("lantern")
	if err != nil {
		return nil, err
	}
	path, name := path.Split(configPath)
	viper.SetConfigName(name)
	viper.AddConfigPath(path)  // path to look for the config file in
	err = viper.ReadInConfig() // Find and read the config file
	if err != nil {
		return nil, fmt.Errorf("Unable to read lantern.yaml: %s", err)
	}
	var cfg *Config = &Config{}
	cfg.applyFlags()
	// TODO: feed with actual data from cloud config
	cfg.ApplyDefaults()
	// TODO: feed actual parameter from yaml to cfg
	err = updateGlobals(cfg)
	if err != nil {
		return nil, err
	}
	t := time.NewTimer(cfg.cloudPollSleepTime())
	go func() {
		for {
			<-t.C
			buf, err := cfg.fetchCloudConfig()
			if err != nil {
				log.Debug(err)
				continue
			}
			if buf == nil {
				log.Debugf("Config unchanged in cloud")
				continue
			}
			if err := cfg.updateFrom(buf); err != nil {
				log.Debug(err)
			}
			t.Reset(cfg.cloudPollSleepTime())
		}
	}()
	return cfg, err
}

// Run runs the configuration system.
func Run(updateHandler func(updated *Config)) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatal(err)
	}
	defer watcher.Close()

	configPath, err := InConfigDir("lantern.yaml")
	if err != nil {
		return err
	}
	err = watcher.Add(configPath)
	if err != nil {
		log.Fatal(err)
	}

	for {
		select {
		case event := <-watcher.Events:
			if event.Op&fsnotify.Write == fsnotify.Write {
				viper.ReadInConfig()
				var cfg *Config = &Config{}
				cfg.applyFlags()
				// TODO: feed with actual data from cloud config
				cfg.ApplyDefaults()
				// TODO: feed actual parameter from yaml to cfg
				// especially trustedcas
				err = updateGlobals(cfg)
				if err != nil {
					continue
				}
				updateHandler(cfg)
			}
		case err := <-watcher.Errors:
			log.Errorf("fsnotify error:", err)
		}
	}

	for {
		// TODO: feed actual parameter from yaml to cfg
		/*next := m.Next()
		nextCfg := next.(*Config)
		err := updateGlobals(nextCfg)
		if err != nil {
			return err
		}
		updateHandler(nextCfg)*/
	}
}

func updateGlobals(cfg *Config) error {
	globals.InstanceId = viper.GetString("instanceid")
	err := globals.SetTrustedCAs(cfg.TrustedCACerts())
	if err != nil {
		return fmt.Errorf("Unable to configure trusted CAs: %s", err)
	}
	return nil
}

// Update updates the configuration using the given mutator function.
func Update(mutate func(cfg *Config) error) error {
	// TODO: use viper.Set()
	return nil /*m.Update(func(ycfg yamlconf.Config) error {
		return mutate(ycfg.(*Config))
	})*/
}

// InConfigDir returns the path to the given filename inside of the configdir.
func InConfigDir(filename string) (string, error) {
	cdir := *configdir

	if cdir == "" {
		if runtime.GOOS == "linux" {
			// It is more common on Linux to expect application related directories
			// in all lowercase. The lantern wrapper also expects a lowercased
			// directory.
			cdir = appdir.General("lantern")
		} else {
			// In OSX and Windows, they prefer to see the first letter in uppercase.
			cdir = appdir.General("Lantern")
		}
	}

	log.Debugf("Placing configuration in %v", cdir)
	if _, err := os.Stat(cdir); err != nil {
		if os.IsNotExist(err) {
			// Create config dir
			if err := os.MkdirAll(cdir, 0750); err != nil {
				return "", fmt.Errorf("Unable to create configdir at %s: %s", cdir, err)
			}
		}
	}

	return filepath.Join(cdir, filename), nil
}

// TrustedCACerts returns a slice of PEM-encoded certs for the trusted CAs
func (cfg *Config) TrustedCACerts() []string {
	certs := make([]string, 0, len(cfg.TrustedCAs))
	for _, ca := range cfg.TrustedCAs {
		certs = append(certs, ca.Cert)
	}
	return certs
}

/*// GetVersion implements the method from interface yamlconf.Config
func (cfg *Config) GetVersion() int {
	return cfg.Version
}

// SetVersion implements the method from interface yamlconf.Config
func (cfg *Config) SetVersion(version int) {
	cfg.Version = version
}

// ApplyDefaults implements the method from interface yamlconf.Config
//
// ApplyDefaults populates default values on a Config to make sure that we have
// a minimum viable config for running.  As new settings are added to
// flashlight, this function should be updated to provide sensible defaults for
// those settings.*/
func (cfg *Config) ApplyDefaults() {
	if cfg.Role == "" {
		cfg.Role = "client"
	}

	if cfg.Addr == "" {
		cfg.Addr = "localhost:8787"
	}

	if cfg.UIAddr == "" {
		cfg.UIAddr = "localhost:16823"
	}

	if cfg.CloudConfig == "" {
		cfg.CloudConfig = "https://config.getiantem.org/cloud.yaml.gz"
	}

	if cfg.InstanceId == "" {
		cfg.InstanceId = hex.EncodeToString(uuid.NodeID())
	}

	// Make sure we always have a stats config
	if cfg.Stats == nil {
		cfg.Stats = &statreporter.Config{}
	}

	if cfg.Stats.StatshubAddr == "" {
		cfg.Stats.StatshubAddr = *statshubAddr
	}

	if cfg.Client != nil && cfg.Role == "client" {
		cfg.applyClientDefaults()
	}

	if cfg.ProxiedSites == nil {
		log.Debugf("Adding empty proxiedsites")
		cfg.ProxiedSites = &proxiedsites.Config{
			Delta: &proxiedsites.Delta{
				Additions: []string{},
				Deletions: []string{},
			},
			Cloud: []string{},
		}
	}

	if cfg.ProxiedSites.Cloud == nil || len(cfg.ProxiedSites.Cloud) == 0 {
		log.Debugf("Loading default cloud proxiedsites")
		cfg.ProxiedSites.Cloud = defaultProxiedSites
	}

	if cfg.TrustedCAs == nil || len(cfg.TrustedCAs) == 0 {
		cfg.TrustedCAs = defaultTrustedCAs
	}
	/*viper.SetDefault("role", "client")
	viper.SetDefault("addr", "localhost:8787")
	viper.SetDefault("cloudconfig", "https://config.getiantem.org/cloud.yaml.gz")
	viper.SetDefault("instanceid", hex.EncodeToString(uuid.NodeID()))
	viper.SetDefault("stats.statshubaddr", "pure-journey-3547.herokuapp.com")
	viper.SetDefault("stats.reportingperiod", "localhost:8787")*/
}

func (cfg *Config) applyClientDefaults() {
	// Make sure we always have at least one masquerade set
	if cfg.Client.MasqueradeSets == nil {
		cfg.Client.MasqueradeSets = make(map[string][]*fronted.Masquerade)
	}
	if len(cfg.Client.MasqueradeSets) == 0 {
		cfg.Client.MasqueradeSets[cloudflare] = cloudflareMasquerades
	}

	// Make sure we always have at least one server
	if cfg.Client.FrontedServers == nil {
		cfg.Client.FrontedServers = make([]*client.FrontedServerInfo, 0)
	}
	if len(cfg.Client.FrontedServers) == 0 && len(cfg.Client.ChainedServers) == 0 {
		cfg.Client.FrontedServers = []*client.FrontedServerInfo{
			&client.FrontedServerInfo{
				Host:           "nl.fallbacks.getiantem.org",
				Port:           443,
				PoolSize:       30,
				MasqueradeSet:  cloudflare,
				MaxMasquerades: 20,
				QOS:            10,
				Weight:         4000,
			},
		}

		cfg.Client.ChainedServers = make(map[string]*client.ChainedServerInfo, len(fallbacks))
		for key, fb := range fallbacks {
			cfg.Client.ChainedServers[key] = fb
		}
	}

	if cfg.AutoReport == nil {
		cfg.AutoReport = new(bool)
		*cfg.AutoReport = true
	}

	if cfg.AutoLaunch == nil {
		cfg.AutoLaunch = new(bool)
		*cfg.AutoLaunch = false
	}

	// Make sure all servers have a QOS and Weight configured
	for _, server := range cfg.Client.FrontedServers {
		if server.QOS == 0 {
			server.QOS = 5
		}
		if server.Weight == 0 {
			server.Weight = 100
		}
		if server.RedialAttempts == 0 {
			server.RedialAttempts = 2
		}
	}

	// Always make sure we have a map of ChainedServers
	if cfg.Client.ChainedServers == nil {
		cfg.Client.ChainedServers = make(map[string]*client.ChainedServerInfo)
	}

	// Sort servers so that they're always in a predictable order
	cfg.Client.SortServers()
}

func (cfg *Config) IsDownstream() bool {
	return cfg.Role == "client"
}

func (cfg *Config) IsUpstream() bool {
	return !cfg.IsDownstream()
}

func (cfg Config) cloudPollSleepTime() time.Duration {
	return time.Duration((CloudConfigPollInterval.Nanoseconds() / 2) + rand.Int63n(CloudConfigPollInterval.Nanoseconds()))
}

func (cfg Config) fetchCloudConfig() ([]byte, error) {
	url := cfg.CloudConfig
	log.Debugf("Checking for cloud configuration at: %s", url)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("Unable to construct request for cloud config at %s: %s", url, err)
	}
	if lastCloudConfigETag[url] != "" {
		// Don't bother fetching if unchanged
		req.Header.Set(ifNoneMatch, lastCloudConfigETag[url])
	}

	// make sure to close the connection after reading the Body
	// this prevents the occasional EOFs errors we're seeing with
	// successive requests
	req.Close = true

	resp, err := httpClient.Load().(*http.Client).Do(req)
	if err != nil {
		return nil, fmt.Errorf("Unable to fetch cloud config at %s: %s", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == 304 {
		return nil, nil
	} else if resp.StatusCode != 200 {
		return nil, fmt.Errorf("Unexpected response status: %d", resp.StatusCode)
	}

	lastCloudConfigETag[url] = resp.Header.Get(etag)
	gzReader, err := gzip.NewReader(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("Unable to open gzip reader: %s", err)
	}
	return ioutil.ReadAll(gzReader)
}

type cloudConfig struct {
	Client       client.ClientConfig
	ProxiedSites proxiedsites.Config
	TrustedCAs   []*CA
}

// updateFrom creates a new Config by 'merging' the given yaml into this Config.
// The masquerade sets, the collections of servers, and the trusted CAs in the
// update yaml  completely replace the ones in the original Config.
func (updated *Config) updateFrom(updateBytes []byte) error {
	ccfg := cloudConfig{
		Client: client.ClientConfig{
			FrontedServers: []*client.FrontedServerInfo{},
			ChainedServers: map[string]*client.ChainedServerInfo{},
			MasqueradeSets: map[string][]*fronted.Masquerade{},
		},
		ProxiedSites: proxiedsites.Config{
			Delta: &proxiedsites.Delta{
				Additions: []string{},
				Deletions: []string{},
			},
			Cloud: []string{},
		},
		TrustedCAs: []*CA{},
	}
	err := yaml.Unmarshal(updateBytes, &ccfg)
	if err != nil {
		return fmt.Errorf("Unable to unmarshal YAML for update: %s", err)
	}
	// Use SetDefault here so same item from local file can override it
	viper.SetDefault("client.chainedservers", ccfg.Client.ChainedServers)
	viper.SetDefault("client.frontedservers", ccfg.Client.FrontedServers)
	viper.SetDefault("client.masqueradesets", ccfg.Client.MasqueradeSets)
	viper.SetDefault("trustedcas", ccfg.TrustedCAs)
	// XXX: does this need a mutex, along with everyone that uses the config?
	oldFrontedServers := updated.Client.FrontedServers
	oldChainedServers := updated.Client.ChainedServers
	oldMasqueradeSets := updated.Client.MasqueradeSets
	oldTrustedCAs := updated.TrustedCAs
	updated.Client.FrontedServers = []*client.FrontedServerInfo{}
	updated.Client.ChainedServers = map[string]*client.ChainedServerInfo{}
	updated.Client.MasqueradeSets = map[string][]*fronted.Masquerade{}

	updated.TrustedCAs = []*CA{}
	err = yaml.Unmarshal(updateBytes, updated)
	if err != nil {
		updated.Client.FrontedServers = oldFrontedServers
		updated.Client.ChainedServers = oldChainedServers
		updated.Client.MasqueradeSets = oldMasqueradeSets
		updated.TrustedCAs = oldTrustedCAs
		return fmt.Errorf("Unable to unmarshal YAML for update: %s", err)
	}
	// Deduplicate global proxiedsites
	if len(updated.ProxiedSites.Cloud) > 0 {
		wlDomains := make(map[string]bool)
		for _, domain := range updated.ProxiedSites.Cloud {
			wlDomains[domain] = true
		}
		updated.ProxiedSites.Cloud = make([]string, 0, len(wlDomains))
		for domain, _ := range wlDomains {
			updated.ProxiedSites.Cloud = append(updated.ProxiedSites.Cloud, domain)
		}
		sort.Strings(updated.ProxiedSites.Cloud)
	}
	return nil
}
