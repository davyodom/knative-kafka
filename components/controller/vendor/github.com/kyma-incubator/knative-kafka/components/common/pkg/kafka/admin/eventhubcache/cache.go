package eventhubcache

import (
	"context"
	"errors"
	"fmt"
	"github.com/kyma-incubator/knative-kafka/components/common/pkg/k8s"
	"github.com/kyma-incubator/knative-kafka/components/common/pkg/kafka/admin/util"
	"github.com/kyma-incubator/knative-kafka/components/common/pkg/kafka/constants"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
)

/* ~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~
   This package implements a cache of the state of Azure EventHub Namespaces.  This is necessary to facilitate the
   usage of the Azure Namespace grouping of EventHubs (which each have their own Connection String / Credentials).
   ~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~ */

// Define An Interface For The EventHub Cache
type CacheInterface interface {
	Update(ctx context.Context) error
	AddEventHub(ctx context.Context, eventhub string, namespace *Namespace)
	RemoveEventHub(ctx context.Context, eventhub string)
	GetNamespace(eventhub string) *Namespace
	GetNamespaceWithMaxCapacity() *Namespace
}

// Verify The Cache Struct Implements The Interface
var _ CacheInterface = &Cache{}

// Azure EventHubs Cache Struct
type Cache struct {
	logger       *zap.Logger
	k8sClient    kubernetes.Interface
	k8sNamespace string
	namespaceMap map[string]*Namespace // Map Of The Azure Namespace Name To Namespace Struct
	eventhubMap  map[string]*Namespace // Maps The Azure EventHub Name To It's Namespace Struct
}

// GetKubernetesClient Wrapper To Facilitate Unit Testing
var GetKubernetesClientWrapper = func(logger *zap.Logger) kubernetes.Interface {
	return k8s.GetKubernetesClient(logger)
}

// Azure EventHubs Cache Constructor
func NewCache(logger *zap.Logger, k8sNamespace string) CacheInterface {

	// Get The Kubernetes Client
	k8sClient := GetKubernetesClientWrapper(logger)

	// Create & Return A New Cache
	return &Cache{
		logger:       logger,
		k8sClient:    k8sClient,
		k8sNamespace: k8sNamespace,
		namespaceMap: make(map[string]*Namespace),
		eventhubMap:  make(map[string]*Namespace),
	}
}

// Update The Cache From K8S & Azure
func (c *Cache) Update(ctx context.Context) error {

	// Get A List Of The Kafka Secrets From The K8S Namespace
	kafkaSecrets, err := util.GetKafkaSecrets(c.k8sClient, c.k8sNamespace)
	if err != nil {
		c.logger.Error("Failed To Get Kafka Secrets", zap.String("Namespace", c.k8sNamespace), zap.Error(err))
		return err
	}

	// Loop Over The Secrets Populating The Cache
	for _, kafkaSecret := range kafkaSecrets.Items {

		// Validate Secret Data
		if !c.validateKafkaSecret(&kafkaSecret) {
			err = errors.New("invalid Kafka Secret found")
			c.logger.Error("Found Invalid Kafka Secret", zap.Any("Kafka Secret", kafkaSecret), zap.Error(err))
			return err
		}

		// Create A New Namespace From The Secret
		namespace := NewNamespaceFromKafkaSecret(&kafkaSecret)

		// Add The Namespace To The Namespace Map
		c.namespaceMap[namespace.Name] = namespace

		// List The EventHubs For The Namespace
		eventHubs, err := namespace.HubManager.List(ctx)
		if err != nil {
			c.logger.Error("Failed To List EventHubs In Namespace", zap.String("Namespace", namespace.Name), zap.Error(err))
			return err
		}

		// Loop Over The EventHubs For The Namespace
		for _, eventHub := range eventHubs {

			// Add The EventHub To The Namespace & Increment The Namespace EventHub Count
			c.eventhubMap[eventHub.Name] = namespace
			namespace.Count = namespace.Count + 1
		}
	}

	// Log Some Basic Cache Information
	c.logger.Info("Updating EventHub Cache",
		zap.Any("Namespaces", c.getNamespaceNames()),
		zap.Any("EventHubs", c.getEventHubNames()),
	)

	// Return Success
	return nil
}

// Add The Specified EventHub / Namespace To The Cache
func (c *Cache) AddEventHub(ctx context.Context, eventhub string, namespace *Namespace) {
	if namespace != nil && namespace.Count < constants.MaxEventHubsPerNamespace {
		namespace.Count = namespace.Count + 1
		c.eventhubMap[eventhub] = namespace
	}
}

// Remove The Specified EventHub / Namespace From The Cache
func (c *Cache) RemoveEventHub(ctx context.Context, eventhub string) {
	namespace := c.GetNamespace(eventhub)
	if namespace != nil && namespace.Count > 0 {
		namespace.Count = namespace.Count - 1
	}
	delete(c.eventhubMap, eventhub)
}

// Get The Namespace Associated With The Specified EventHub (Topic) Name
func (c *Cache) GetNamespace(eventhub string) *Namespace {
	return c.eventhubMap[eventhub]
}

// Get A Namespace With The Maximum Available Capacity (Load Balanced Across Namespaces ;)
func (c *Cache) GetNamespaceWithMaxCapacity() *Namespace {

	// Track The Namespace With The Most Capacity
	var maxCapacityNamespace *Namespace

	// Loop Over The Namespaces In The Map
	for _, namespace := range c.namespaceMap {

		// Skip Any Invalid Data (Precautionary - Shouldn't Happen)
		if namespace == nil {
			continue
		}

		// Initialize The Max Capacity Namespace If Not Set
		if maxCapacityNamespace == nil && namespace.Count < constants.MaxEventHubsPerNamespace {
			maxCapacityNamespace = namespace
			continue
		}

		// If The MaxCapacityNamespace Has Been Set
		if maxCapacityNamespace != nil {

			// Stop Looking If We Have An Empty Namespace (Can't get any more capacity than that ; )
			if maxCapacityNamespace.Count == 0 {
				break
			}

			// Update The Max Capacity Namespace If Current Has More Capacity
			if namespace.Count < maxCapacityNamespace.Count {
				maxCapacityNamespace = namespace
			}
		}
	}

	// Log The Max Capacity Namespace
	if maxCapacityNamespace != nil {
		c.logger.Info("Max Capacity Namespace Lookup",
			zap.String("Namespace", maxCapacityNamespace.Name),
			zap.Int("Capacity", constants.MaxEventHubsPerNamespace-maxCapacityNamespace.Count),
		)
	} else {
		c.logger.Warn("Found No Azure Namespace With Available Capacity!")
	}

	// Return The Max Capacity Namespace
	return maxCapacityNamespace
}

// Utility Function For Validating Kafka Secret
func (c *Cache) validateKafkaSecret(secret *corev1.Secret) bool {

	// Assume Invalid Until Proven Otherwise
	valid := false

	// Validate The Kafka Secret
	if secret != nil {

		// Extract The Relevant Data From The Kafka Secret
		brokers := string(secret.Data[constants.KafkaSecretKeyBrokers])
		username := string(secret.Data[constants.KafkaSecretKeyUsername])
		password := string(secret.Data[constants.KafkaSecretKeyPassword])
		namespace := string(secret.Data[constants.KafkaSecretKeyNamespace])

		// Validate Kafka Secret Data
		if len(brokers) > 0 && len(username) > 0 && len(password) > 0 && len(namespace) > 0 {

			// Mark Kafka Secret As Valid
			valid = true

		} else {

			// Invalid Kafka Secret - Log State
			pwdString := ""
			if len(password) > 0 {
				pwdString = "********"
			}
			c.logger.Error("Kafka Secret Contains Invalid Data",
				zap.String("Name", secret.Name),
				zap.String("Brokers", brokers),
				zap.String("Username", username),
				zap.String("Password", pwdString),
				zap.String("Namespace", namespace))
		}
	}

	// Return Kafka Secret Validity
	return valid
}

// Utility Function For Getting The Namespace "Name (Count)" As A []string
func (c *Cache) getNamespaceNames() []string {
	namespaceNames := make([]string, len(c.namespaceMap))
	for namespaceName, namespace := range c.namespaceMap {
		namespaceNames = append(namespaceNames, fmt.Sprintf("%s (%d)", namespaceName, namespace.Count))
	}
	return namespaceNames
}

// Utility Function For Getting The "EventHub Name -> Namespace Name" As A []string
func (c *Cache) getEventHubNames() []string {
	eventHubNames := make([]string, len(c.eventhubMap))
	for eventHubName, namespace := range c.eventhubMap {
		eventHubNames = append(eventHubNames, fmt.Sprintf("%s -> %s", eventHubName, namespace.Name))
	}
	return eventHubNames
}
