package main

import (
	"fmt"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

type DroveConfig struct {
	Name        string
	Drove       []string `json:"-"`
	User        string   `json:"-"`
	Pass        string   `json:"-"`
	AccessToken string   `json:"-" toml:"access_token"`
	Realm       string
	RealmSuffix string `json:"-" toml:"realm_suffix"`
	RoutingTag  string `json:"-" toml:"routing_tag"`
	LeaderVHost string `json:"-" toml:"leader_vhost"`
}

// NamespaceData holds the data and metadata for each namespace
type NamespaceData struct {
	Drove       DroveConfig
	Leader      LeaderController
	Apps        map[string]App
	KnownVHosts Vhosts
	Timestamp   time.Time // Timestamp of creation or modification
}

type StaticConfig struct {
	Xproxy              string
	LeftDelimiter       string `json:"-" toml:"left_delimiter"`
	RightDelimiter      string `json:"-" toml:"right_delimiter"`
	MaxFailsUpstream    *int   `json:"max_fails,omitempty"`
	FailTimeoutUpstream string `json:"fail_timeout,omitempty"`
	SlowStartUpstream   string `json:"slow_start,omitempty"`
}

// DataManager manages namespaces and data for those namespaces
type DataManager struct {
	mu                  sync.RWMutex             // Mutex for concurrency control
	namespaces          map[string]NamespaceData // Map of namespaces to NamespaceData
	LastKnownVhosts     Vhosts
	LastReloadTimestamp time.Time // Timestamp of creation or modification
	StaticData          StaticConfig
}

// NewDataManager creates a new instance of DataManager
func NewDataManager(inXproxy string, inLeftDelimiter string, inRightDelimiter string,
	inMaxFailsUpstream *int, inFailTimeoutUpstream string, inSlowStartUpstream string) *DataManager {
	empltyLastKnownVhosts := Vhosts{}
	empltyLastKnownVhosts.Vhosts = make(map[string]bool)
	return &DataManager{
		namespaces: make(map[string]NamespaceData),
		StaticData: StaticConfig{Xproxy: inXproxy, LeftDelimiter: inLeftDelimiter, RightDelimiter: inRightDelimiter,
			MaxFailsUpstream: inMaxFailsUpstream, FailTimeoutUpstream: inFailTimeoutUpstream, SlowStartUpstream: inSlowStartUpstream},
		LastKnownVhosts: empltyLastKnownVhosts, LastReloadTimestamp: time.Now(),
	}
}

// Create inserts data into a namespace
func (dm *DataManager) CreateNamespace(namespace string, inDrove []string, inUser string, inPass string, inAccessToken string,
	inRealm string, inRealmSuffix string, inRoutingTag string, inLeaderVhost string) error {
	dm.mu.Lock()         // Lock to ensure concurrent writes are handled
	defer dm.mu.Unlock() // Ensure the lock is always released

	// Start the operation log
	logger.WithFields(logrus.Fields{
		"operation": "create",
		"namespace": namespace,
	}).Info("Attempting to create Namespace")

	// Ensure namespace exists
	if _, exists := dm.namespaces[namespace]; !exists {
		dm.namespaces[namespace] = NamespaceData{
			Drove: DroveConfig{
				Name:        namespace,
				Drove:       inDrove,
				User:        inUser,
				Pass:        inPass,
				AccessToken: inAccessToken,
				Realm:       inRealm,
				RealmSuffix: inRealmSuffix,
				RoutingTag:  inRoutingTag,
				LeaderVHost: inLeaderVhost,
			},
			Timestamp: time.Now(),
		}
	}

	// Log success
	logger.WithFields(logrus.Fields{
		"operation": "create",
		"namespace": namespace,
	}).Info("Namespace created successfully")
	return nil
}

func (dm *DataManager) ReadDroveConfig(namespace string) (DroveConfig, error) {
	dm.mu.RLock()         // Read lock to allow multiple concurrent reads
	defer dm.mu.RUnlock() // Ensure the lock is always released
	operation := "ReadDroveConfig"

	// Ensure namespace exists
	if _, exists := dm.namespaces[namespace]; !exists {
		logger.WithFields(logrus.Fields{
			"operation": operation,
			"namespace": namespace,
		}).Error("NamespaceData read failed")
		err := fmt.Errorf("namespace '%s' not found", namespace)
		return DroveConfig{}, err
	}

	ns := dm.namespaces[namespace]

	// Log success
	logger.WithFields(logrus.Fields{
		"operation": operation,
		"namespace": namespace,
	}).Debug("ReadDroveConfig successfully")

	return ns.Drove, nil //returning copy
}

func (dm *DataManager) ReadLastTimestamp(namespace string) (time.Time, error) {
	dm.mu.RLock()         // Read lock to allow multiple concurrent reads
	defer dm.mu.RUnlock() // Ensure the lock is always released
	operation := "ReadLastTimestamps"

	// Ensure namespace exists
	if _, exists := dm.namespaces[namespace]; !exists {
		logger.WithFields(logrus.Fields{
			"operation": operation,
			"namespace": namespace,
		}).Error("NamespaceData read failed")
		err := fmt.Errorf("namespace '%s' not found", namespace)
		return time.Time{}, err
	}

	ns := dm.namespaces[namespace]

	// Log success
	logger.WithFields(logrus.Fields{
		"operation": operation,
		"namespace": namespace,
		"Timestamp": ns.Timestamp,
	}).Info("ReadLastTimestamps successfully")

	return ns.Timestamp, nil //returning copy
}

func (dm *DataManager) ReadLeader(namespace string) (LeaderController, error) {
	dm.mu.RLock()         // Read lock to allow multiple concurrent reads
	defer dm.mu.RUnlock() // Ensure the lock is always released
	operation := "ReadLeader"

	// Ensure namespace exists
	if _, exists := dm.namespaces[namespace]; !exists {
		logger.WithFields(logrus.Fields{
			"operation": operation,
			"namespace": namespace,
		}).Error("NamespaceData read failed")
		err := fmt.Errorf("namespace '%s' not found", namespace)
		return LeaderController{}, err
	}

	ns := dm.namespaces[namespace]

	// Log success
	logger.WithFields(logrus.Fields{
		"operation": operation,
		"namespace": namespace,
		"leader":    ns.Leader,
	}).Info("ReadLeader successfully")

	return ns.Leader, nil //returning copy
}

func (dm *DataManager) ReadAllLeaders() []LeaderController {
	dm.mu.RLock()         // Read lock to allow multiple concurrent reads
	defer dm.mu.RUnlock() // Ensure the lock is always released
	operation := "ReadAllLeaders"

	// Start the operation log
	logger.WithFields(logrus.Fields{
		"operation": operation,
	}).Debug("Attempting to read namespace data")

	var allLeaders []LeaderController

	for _, data := range dm.namespaces {
		allLeaders = append(allLeaders, data.Leader)
	}
	// Log success
	logger.WithFields(logrus.Fields{
		"operation": operation,
		"allLeader": allLeaders,
	}).Info("ReadAllLeaders successfully")

	return allLeaders //returning copy
}

func (dm *DataManager) UpdateLeader(namespace string, leader LeaderController) error {
	dm.mu.Lock()         // Read lock to allow multiple concurrent reads
	defer dm.mu.Unlock() // Ensure the lock is always released
	operation := "UpdateLeader"

	// Ensure namespace exists
	if _, exists := dm.namespaces[namespace]; !exists {
		logger.WithFields(logrus.Fields{
			"operation": operation,
			"namespace": namespace,
		}).Error("NamespaceData read failed")
		err := fmt.Errorf("namespace '%s' not found", namespace)
		return err
	}

	ns := dm.namespaces[namespace]
	ns.Leader = leader
	ns.Timestamp = time.Now() // Update timestamp on modification
	dm.namespaces[namespace] = ns

	logger.WithFields(logrus.Fields{
		"operation": operation,
		"namespace": namespace,
		"leader":    ns.Leader,
		"time":      ns.Timestamp,
	}).Info("UpdateLeader data successfully")
	return nil
}

func (dm *DataManager) ReadApps(namespace string) (map[string]App, error) {
	dm.mu.RLock()         // Read lock to allow multiple concurrent reads
	defer dm.mu.RUnlock() // Ensure the lock is always released
	operation := "ReadLeader"

	// Ensure namespace exists
	if _, exists := dm.namespaces[namespace]; !exists {
		logger.WithFields(logrus.Fields{
			"operation": operation,
			"namespace": namespace,
		}).Error("NamespaceData read failed")
		err := fmt.Errorf("namespace '%s' not found", namespace)
		return map[string]App{}, err
	}

	ns := dm.namespaces[namespace]

	// Log success
	logger.WithFields(logrus.Fields{
		"operation": operation,
		"namespace": namespace,
		"apps":      ns.Apps,
	}).Info("ReadApp successfully")

	return ns.Apps, nil //returning copy
}

func (dm *DataManager) ReadAllApps() map[string]App {
	dm.mu.RLock()         // Read lock to allow multiple concurrent reads
	defer dm.mu.RUnlock() // Ensure the lock is always released
	operation := "ReadAllApps"

	allApps := make(map[string]App)

	for _, data := range dm.namespaces {
		for appId, appData := range data.Apps {
			allApps[appId] = appData
		}
	}
	// Log success
	logger.WithFields(logrus.Fields{
		"operation": operation,
		"allApps":   allApps,
	}).Info("ReadAllApps successfully")

	return allApps //returning copy

}

func (dm *DataManager) UpdateApps(namespace string, apps map[string]App) error {
	dm.mu.Lock()         // Read lock to allow multiple concurrent reads
	defer dm.mu.Unlock() // Ensure the lock is always released
	operation := "UpdateApps"

	// Ensure namespace exists
	if _, exists := dm.namespaces[namespace]; !exists {
		logger.WithFields(logrus.Fields{
			"operation": operation,
			"namespace": namespace,
		}).Error("NamespaceData read failed")
		err := fmt.Errorf("namespace '%s' not found", namespace)
		return err
	}

	ns := dm.namespaces[namespace]
	ns.Apps = apps
	ns.Timestamp = time.Now() // Update timestamp on modification
	dm.namespaces[namespace] = ns

	logger.WithFields(logrus.Fields{
		"operation": operation,
		"namespace": namespace,
		"apps":      dm.namespaces[namespace].Apps,
		"time":      dm.namespaces[namespace].Timestamp,
	}).Info("UpdateApps successfully")
	return nil
}

func (dm *DataManager) ReadKnownVhosts(namespace string) (Vhosts, error) {
	dm.mu.RLock()         // Read lock to allow multiple concurrent reads
	defer dm.mu.RUnlock() // Ensure the lock is always released
	operation := "ReadKnownVhosts"

	// Ensure namespace exists
	if _, exists := dm.namespaces[namespace]; !exists {
		logger.WithFields(logrus.Fields{
			"operation": operation,
			"namespace": namespace,
		}).Error("NamespaceData read failed")
		err := fmt.Errorf("namespace '%s' not found", namespace)
		return Vhosts{}, err
	}

	ns := dm.namespaces[namespace]

	// Log success
	logger.WithFields(logrus.Fields{
		"operation":   operation,
		"namespace":   namespace,
		"knownVHosts": ns.KnownVHosts,
	}).Info("ReadKnownVhosts successfully")

	return ns.KnownVHosts, nil //returning copy
}

func (dm *DataManager) ReadAllKnownVhosts() Vhosts {
	dm.mu.RLock()         // Read lock to allow multiple concurrent reads
	defer dm.mu.RUnlock() // Ensure the lock is always released
	operation := "ReadAllKnownVhosts"

	allKnownVhosts := Vhosts{}
	allKnownVhosts.Vhosts = make(map[string]bool)

	for _, data := range dm.namespaces {
		for key, value := range data.KnownVHosts.Vhosts {
			allKnownVhosts.Vhosts[key] = value
		}
	}

	// Log success
	logger.WithFields(logrus.Fields{
		"operation": operation,
		"allApps":   allKnownVhosts,
	}).Info("ReadAllKnownVhosts successfully")

	return allKnownVhosts //returning copy
}

func (dm *DataManager) UpdateKnownVhosts(namespace string, KnownVHosts Vhosts) error {
	dm.mu.Lock()         // Read lock to allow multiple concurrent reads
	defer dm.mu.Unlock() // Ensure the lock is always released
	operation := "UpdateKnownVhosts"

	// Ensure namespace exists
	if _, exists := dm.namespaces[namespace]; !exists {
		logger.WithFields(logrus.Fields{
			"operation": operation,
			"namespace": namespace,
		}).Error("NamespaceData read failed")
		err := fmt.Errorf("namespace '%s' not found", namespace)
		return err
	}

	ns := dm.namespaces[namespace]
	ns.KnownVHosts = KnownVHosts
	ns.Timestamp = time.Now() // Update timestamp on modification
	dm.namespaces[namespace] = ns

	logger.WithFields(logrus.Fields{
		"operation":  operation,
		"namespace":  namespace,
		"knownHosts": dm.namespaces[namespace].KnownVHosts,
		"time":       dm.namespaces[namespace].Timestamp,
	}).Info("UpdateKnownVhosts successfully")
	return nil
}

func (dm *DataManager) ReadLastReloadTimestamp() time.Time {
	dm.mu.RLock()         // Read lock to allow multiple concurrent reads
	defer dm.mu.RUnlock() // Ensure the lock is always released
	operation := "LastReloadTimestamp"

	// Log success
	logger.WithFields(logrus.Fields{
		"operation":           operation,
		"LastReloadTimestamp": dm.LastReloadTimestamp,
	}).Info("ReadLoadedTime successfully")

	return dm.LastReloadTimestamp //returning copy
}

func (dm *DataManager) ReadLastKnownVhosts() Vhosts {
	dm.mu.RLock()         // Read lock to allow multiple concurrent reads
	defer dm.mu.RUnlock() // Ensure the lock is always released
	operation := "ReadLastKnownVhosts"

	// Log success
	logger.WithFields(logrus.Fields{
		"operation":       operation,
		"LastKnownVhosts": dm.LastKnownVhosts,
	}).Info("LastKnownVhosts successfully")

	return dm.LastKnownVhosts //returning copy
}

func (dm *DataManager) UpdateLastKnownVhosts(inLastKnownVhosts Vhosts) error {
	dm.mu.Lock()         // Read lock to allow multiple concurrent reads
	defer dm.mu.Unlock() // Ensure the lock is always released
	operation := "UpdateLastKnownVhosts"

	dm.LastKnownVhosts = inLastKnownVhosts
	dm.LastReloadTimestamp = time.Now()

	logger.WithFields(logrus.Fields{
		"operation":       operation,
		"LastKnownVhosts": dm.LastKnownVhosts,
	}).Info("UpdateLastKnownVhosts successfully")
	return nil
}

func (dm *DataManager) ReadAllNamespace() map[string]NamespaceData {
	dm.mu.RLock()         // Read lock to allow multiple concurrent reads
	defer dm.mu.RUnlock() // Ensure the lock is always released
	operation := "ReadAllNamespace"

	// Start the operation log
	logger.WithFields(logrus.Fields{
		"operation": operation,
	}).Info("ReadAllNamespace data successfully")

	return dm.namespaces //returning copy
}

// Read retrieves data from a specific namespace
func (dm *DataManager) ReadStaticData() StaticConfig {
	dm.mu.RLock()         // Read lock to allow multiple concurrent reads
	defer dm.mu.RUnlock() // Ensure the lock is always released
	operation := "ReadStaticData"

	// Start the operation log
	logger.WithFields(logrus.Fields{
		"operation":  operation,
		"staticData": dm.StaticData,
	}).Info("ReadStaticData successfully")

	return dm.StaticData //returning copy
}
