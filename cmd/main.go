// Copyright 2020 The Cloud Native Events Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package main ...
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/redhat-cne/sdk-go/pkg/subscriber"
	v1pubs "github.com/redhat-cne/sdk-go/v1/pubsub"

	"github.com/redhat-cne/sdk-go/pkg/types"

	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/redhat-cne/sdk-go/pkg/util/wait"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redhat-cne/cloud-event-proxy/pkg/localmetrics"
	storageClient "github.com/redhat-cne/cloud-event-proxy/pkg/storage/kubernetes"

	log "github.com/sirupsen/logrus"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redhat-cne/cloud-event-proxy/pkg/common"
	"github.com/redhat-cne/cloud-event-proxy/pkg/plugins"
	"github.com/redhat-cne/cloud-event-proxy/pkg/restclient"
	apiMetrics "github.com/redhat-cne/rest-api/pkg/localmetrics"
	"github.com/redhat-cne/sdk-go/pkg/channel"
	sdkMetrics "github.com/redhat-cne/sdk-go/pkg/localmetrics"
	v1event "github.com/redhat-cne/sdk-go/v1/event"
	subscriberApi "github.com/redhat-cne/sdk-go/v1/subscriber"
)

const (
	configMapRetryInterval = 3 * time.Second
	configMapRetryCount    = 5
)

var (
	// defaults
	storePath               string
	transportHost           string
	apiPort                 int
	apiVersion              string
	channelBufferSize       = 100
	statusChannelBufferSize = 50
	scConfig                *common.SCConfiguration
	metricsAddr             string
	apiPath                 = "/api/ocloudNotifications/v2/"
	pluginHandler           plugins.Handler
	nodeName                string
	namespace               string

	// Git commit of current build set at build time
	GitCommit = "Undefined"
)

func getMajorVersion(version string) (int, error) {
	if version == "" {
		return 1, nil
	}
	version = strings.TrimPrefix(version, "v")
	version = strings.TrimPrefix(version, "V")
	v := strings.Split(version, ".")
	majorVersion, err := strconv.Atoi(v[0])
	if err != nil {
		log.Errorf("Error parsing major version from %s, %v", version, err)
		return 1, err
	}
	return majorVersion, nil
}

func isV1Api(version string) bool {
	if majorVersion, err := getMajorVersion(version); err == nil {
		if majorVersion >= 2 {
			return false
		}
	}
	return true
}

func main() {
	fmt.Printf("Git commit: %s\n", GitCommit)
	// init
	common.InitLogger()
	flag.StringVar(&metricsAddr, "metrics-addr", ":9091", "The address the metric endpoint binds to.")
	flag.StringVar(&storePath, "store-path", ".", "The path to store publisher and subscription info.")
	// TODO: Rename transportHost to apiHost, which requires changes in PTP Operator.
	flag.StringVar(&transportHost, "transport-host", "http://ptp-event-publisher-service-NODE_NAME.openshift-ptp.svc.cluster.local:9043", "The transport bus hostname or service name.")
	flag.IntVar(&apiPort, "api-port", 9043, "The address the rest api endpoint binds to.")
	flag.StringVar(&apiVersion, "api-version", "2.0", "The address the rest api endpoint binds to.")

	flag.Parse()

	// Register metrics
	localmetrics.RegisterMetrics()
	apiMetrics.RegisterMetrics()
	sdkMetrics.RegisterMetrics()

	// Including these stats kills performance when Prometheus polls with multiple targets
	prometheus.Unregister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	prometheus.Unregister(collectors.NewGoCollector())

	nodeIP := os.Getenv("NODE_IP")
	nodeName = os.Getenv("NODE_NAME")
	namespace = os.Getenv("NAME_SPACE")
	transportHost = common.SanitizeTransportHost(transportHost, nodeIP, nodeName)
	parsedTransportHost := &common.TransportHost{URL: transportHost}

	parsedTransportHost.ParseTransportHost()
	if parsedTransportHost.Err != nil {
		log.Errorf("error parsing transport host, data will written to log %s", parsedTransportHost.Err.Error())
	}
	scConfig = &common.SCConfiguration{
		EventInCh:     make(chan *channel.DataChan, channelBufferSize),
		EventOutCh:    make(chan *channel.DataChan, channelBufferSize),
		StatusCh:      make(chan *channel.StatusChan, statusChannelBufferSize),
		CloseCh:       make(chan struct{}),
		APIPort:       apiPort,
		APIPath:       apiPath,
		StorePath:     storePath,
		PubSubAPI:     v1pubs.GetAPIInstance(storePath),
		SubscriberAPI: subscriberApi.GetAPIInstance(storePath),
		BaseURL:       nil,
		TransportHost: parsedTransportHost,
		StorageType:   storageClient.EmptyDir,
	}
	/****/

	// Use kubeconfig to create client config.
	client, err := storageClient.NewClient()
	if err != nil {
		log.Infof("error fetching client, storage defaulted to emptyDir{} %s", err.Error())
	} else {
		scConfig.K8sClient = client
	}
	if namespace != "" && nodeName != "" && scConfig.TransportHost.Type == common.HTTP {
		// if consumer doesn't pass namespace then this will default to empty dir
		if e := client.InitConfigMap(scConfig.StorePath, nodeName, namespace, configMapRetryInterval, configMapRetryCount); e != nil {
			log.Errorf("failed to initialize configmap, subscription will be stored in empty dir %s", e.Error())
		} else {
			scConfig.StorageType = storageClient.ConfigMap
		}
	}
	metricServer(metricsAddr)
	wg := sync.WaitGroup{}
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)

	go func() {
		<-sigCh
		log.Info("exiting...")
		close(scConfig.CloseCh)
		os.Exit(1)
	}()

	pluginHandler = plugins.Handler{Path: "./plugins"}
	if isV1Api(apiVersion) {
		log.Fatal("REST API v1 is no longer supported. " +
			"Please update the API version to 2.0.")
	}
	log.Infof(
		"REST API config: version=%s, port=%d, path=%s.",
		apiVersion,
		scConfig.APIPort,
		scConfig.APIPath)

	// Enable pub/sub services
	err = common.StartPubSubService(scConfig)
	if err != nil {
		log.Fatal("pub/sub service API failed to start.")
	}

	// assume this depends on rest plugin, or you can use api to create subscriptions
	if common.GetBoolEnv("PTP_PLUGIN") {
		if ptpPluginError := pluginHandler.LoadPTPPlugin(&wg, scConfig, nil); ptpPluginError != nil {
			log.Fatalf("error loading ptp plugin %v", ptpPluginError)
		}
	}

	if common.GetBoolEnv("MOCK_PLUGIN") {
		if mPluginError := pluginHandler.LoadMockPlugin(&wg, scConfig, nil); mPluginError != nil {
			log.Fatalf("error loading mock plugin %v", err)
		}
	}

	// process data that are coming from api server requests
	ProcessOutChannel(&wg, scConfig)
}

func metricServer(address string) {
	log.Info("starting metrics")
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())

	go wait.Until(func() {
		server := &http.Server{
			Addr:              address,
			ReadHeaderTimeout: 5 * time.Second,
			Handler:           mux,
		}
		err := server.ListenAndServe()
		if err != nil {
			log.Errorf("error with metrics server %s\n, will retry to establish", err.Error())
		}
	}, 5*time.Second, scConfig.CloseCh)
}

// ProcessOutChannel this process the out channel;data put out by transport
func ProcessOutChannel(wg *sync.WaitGroup, scConfig *common.SCConfiguration) {
	// Send back the acknowledgement to publisher
	defer wg.Done()
	postProcessFn := func(address string, status channel.Status) {
		if pub, ok := scConfig.PubSubAPI.HasPublisher(address); ok {
			if status == channel.SUCCESS {
				localmetrics.UpdateEventAckCount(address, localmetrics.SUCCESS)
			} else {
				localmetrics.UpdateEventAckCount(address, localmetrics.FAILED)
			}
			if pub.EndPointURI != nil {
				log.Debugf("posting acknowledgment with status: %s to publisher: %s", status, pub.EndPointURI)
				restClient := restclient.New()
				if _, err := restClient.Post(pub.EndPointURI,
					[]byte(fmt.Sprintf(`{eventId:"%s",status:"%s"}`, pub.ID, status))); err != nil {
					log.Errorf("error posting acknowledgment at %s : %s", pub.EndPointURI, err)
				}
			}
		}
	}
	postHandler := func(err error, endPointURI *types.URI, address string) {
		if err != nil {
			log.Errorf("error posting request at %s : %s", endPointURI, err)
			localmetrics.UpdateEventReceivedCount(address, localmetrics.FAILED)
		} else {
			localmetrics.UpdateEventReceivedCount(address, localmetrics.SUCCESS)
		}
	}

	for { //nolint:gosimple
		select { //nolint:gosimple
		case d := <-scConfig.EventOutCh: // do something that is put out by transporter
			if d.Type == channel.EVENT {
				if d.Data == nil {
					log.Errorf("nil event data was sent via event channel,ignoring")
					continue
				}
				event, err := v1event.GetCloudNativeEvents(*d.Data)
				if err != nil {
					log.Errorf("error marshalling event data when reading from transport %v\n %#v", err, d)
					log.Infof("data %#v", d.Data)
					continue
				} else if d.Status == channel.NEW {
					if d.ProcessEventFn != nil { // always leave event to handle by default method for events
						if err = d.ProcessEventFn(event); err != nil {
							log.Errorf("error processing data %v", err)
							localmetrics.UpdateEventReceivedCount(d.Address, localmetrics.FAILED)
						}
					} else if sub, ok := scConfig.PubSubAPI.HasSubscription(d.Address); ok {
						// V1 only
						if sub.EndPointURI != nil {
							restClient := restclient.New()
							event.ID = sub.ID // set ID to the subscriptionID
							err = restClient.PostEvent(sub.EndPointURI, event)
							postHandler(err, sub.EndPointURI, d.Address)
						} else {
							log.Warnf("endpoint uri not given, posting event to log %#v for address %s\n", event, d.Address)
							localmetrics.UpdateEventReceivedCount(d.Address, localmetrics.SUCCESS)
						}
					} else {
						// V2
						eventSubscribers := scConfig.SubscriberAPI.GetClientIDAddressByResource(d.Address)
						if len(eventSubscribers) != 0 {
							restClient := restclient.New()
							for clientID, endPointURI := range eventSubscribers {
								if endPointURI != nil {
									log.Infof("post events %s to subscriber %s", d.Address, endPointURI)
									// make sure event ID is unique
									event.ID = uuid.New().String()
									var status, numSubDeleted int
									status, err = restClient.PostCloudEvent(endPointURI, *d.Data)
									if err != nil {
										log.Errorf("error posting event at %s : %s", endPointURI, err)
										localmetrics.UpdateEventReceivedCount(d.Address, localmetrics.FAILED)

										// POST return DNSError if server is not reachable
										var dnsError *net.DNSError
										// Capture DNS error "lookup consumer-events-subscription-service.cloud-events.svc.cluster.local: no such host"
										// or timeout error "context deadline exceeded (Client.Timeout exceeded while awaiting headers)"
										// or connection refused error "dial tcp <ip>:9043: connect: connection refused"
										if errors.As(err, &dnsError) || os.IsTimeout(err) || errors.Is(err, syscall.ECONNREFUSED) {
											// has subscriber failed to connect for 10 times delete the subscribers
											if scConfig.SubscriberAPI.IncFailCountToFail(clientID) {
												log.Errorf("connection lost for address %s, proceed to clean up subscription", d.Address)
												if numSubDeleted, err = scConfig.SubscriberAPI.DeleteAllSubscriptionsForClient(clientID); err != nil {
													log.Errorf("failed to delete all subscriptions for client %s: %s", clientID.String(), err.Error())
												} else {
													cleanupConfigMap(d.ClientID)
												}
												apiMetrics.UpdateSubscriptionCount(apiMetrics.ACTIVE, -(numSubDeleted))
											} else {
												log.Errorf("client %s not responding, waiting %d times before marking to delete subscriber",
													d.Address, scConfig.SubscriberAPI.FailCountThreshold()-scConfig.SubscriberAPI.GetFailCount(clientID))
											}
										}
									} else {
										scConfig.SubscriberAPI.ResetFailCount(clientID)
										if status == http.StatusNoContent {
											localmetrics.UpdateEventReceivedCount(d.Address, localmetrics.SUCCESS)
										} else {
											log.Errorf("posting event at %s returned invalid status %d", endPointURI, status)
											localmetrics.UpdateEventReceivedCount(d.Address, localmetrics.FAILED)
										}
									}
								} else {
									// this should not happen
									log.Errorf("endPointURI is nil for ResourceAddress %s clientID %s", d.Address, clientID)
									continue
								}
							}
						} else {
							log.Warnf("subscription not found, posting event to log for address %s", d.Address)
							localmetrics.UpdateEventReceivedCount(d.Address, localmetrics.FAILED)
						}
					}
				} else if d.Status == channel.SUCCESS || d.Status == channel.FAILED { // event sent ,ack back to publisher
					postProcessFn(d.Address, d.Status)
				}
			} else if d.Type == channel.STATUS {
				if d.Status == channel.SUCCESS {
					localmetrics.UpdateStatusAckCount(d.Address, localmetrics.SUCCESS)
				} else {
					log.Errorf("failed to receive status request to address %s", d.Address)
					localmetrics.UpdateStatusAckCount(d.Address, localmetrics.FAILED)
				}
			} else if d.Type == channel.SUBSCRIBER { // these data are provided by HTTP transport
				if scConfig.StorageType != storageClient.ConfigMap {
					continue
				}
				if d.Status == channel.SUCCESS && d.Data != nil {
					var obj subscriber.Subscriber
					if err := json.Unmarshal(d.Data.Data(), &obj); err != nil {
						log.Infof("data is not subscriber object ignoring processing")
						continue
					}
					log.Infof("subscriber processed for %s", d.Address)
					if err := scConfig.K8sClient.UpdateConfigMap(context.Background(), []subscriber.Subscriber{obj}, nodeName, namespace); err != nil {
						log.Errorf("failed to update subscription in configmap %s", err.Error())
					} else {
						log.Infof("subscriber saved in configmap %s", obj.String())
					}
				} else if d.Status == channel.DELETE {
					cleanupConfigMap(d.ClientID)
				}
			}
		case <-scConfig.CloseCh:
			return
		}
	}
}

// ProcessInChannel will be called if Transport is disabled
func ProcessInChannel(wg *sync.WaitGroup, scConfig *common.SCConfiguration) {
	defer wg.Done()
	for { //nolint:gosimple
		select {
		case d := <-scConfig.EventInCh:
			if d.Type == channel.SUBSCRIBER {
				log.Warnf("event transport disabled,no action taken: request to create listener address %s was called,but transport is not enabled", d.Address)
			} else if d.Type == channel.PUBLISHER {
				log.Warnf("no action taken: request to create sender for address %s was called,but transport is not enabled", d.Address)
			} else if d.Type == channel.EVENT && d.Status == channel.NEW {
				if e, err := v1event.GetCloudNativeEvents(*d.Data); err != nil {
					log.Warnf("error marshalling event data")
				} else {
					log.Warnf("event disabled,no action taken(can't send to a desitination): logging new event %s\n", e.JSONString())
				}
				out := channel.DataChan{
					Address:        d.Address,
					Data:           d.Data,
					Status:         channel.SUCCESS,
					Type:           channel.EVENT,
					ProcessEventFn: d.ProcessEventFn,
				}
				if d.OnReceiveOverrideFn != nil {
					if err := d.OnReceiveOverrideFn(*d.Data, &out); err != nil {
						log.Errorf("error onReceiveOverrideFn %s", err)
						out.Status = channel.FAILED
					} else {
						out.Status = channel.SUCCESS
					}
				}
				scConfig.EventOutCh <- &out
			} else if d.Type == channel.STATUS && d.Status == channel.NEW {
				log.Warnf("event disabled,no action taken(can't send to a destination): logging new status check %v\n", d)
				out := channel.DataChan{
					Address:        d.Address,
					Data:           d.Data,
					Status:         channel.SUCCESS,
					Type:           channel.EVENT,
					ProcessEventFn: d.ProcessEventFn,
				}
				if d.OnReceiveOverrideFn != nil {
					if err := d.OnReceiveOverrideFn(*d.Data, &out); err != nil {
						log.Errorf("error onReceiveOverrideFn %s", err)
						out.Status = channel.FAILED
					} else {
						out.Status = channel.SUCCESS
					}
				}
			}
		case <-scConfig.CloseCh:
			return
		}
	}
}

func cleanupConfigMap(clientID uuid.UUID) {
	var obj subscriber.Subscriber
	obj.Action = channel.DELETE
	obj.ClientID = clientID
	if err := scConfig.K8sClient.UpdateConfigMap(context.Background(), []subscriber.Subscriber{obj}, nodeName, namespace); err != nil {
		log.Errorf("failed to delete subscription in configmap %s", err.Error())
	} else {
		log.Infof("deleted subscription %s ", obj.ClientID.String())
	}
}
