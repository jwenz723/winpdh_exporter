package main

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/lxn/win"
	"github.com/prometheus/client_golang/prometheus"
	log "github.com/sirupsen/logrus"
)

// PdhCounter defines a PDH counter object using a PDH path like: \PDH Category(PDH Instance)\PDH Counter
type PdhCounter struct {
	Path string
}

// TestEquivalence will test if a is equal to p
func (p *PdhCounter) TestEquivalence(a *PdhCounter) bool {
	return p.Path == a.Path
}

// PdhCounterSet defines a PdhCounter set to be collected on a single Host
type PdhCounterSet struct {
	completedInitialization bool // Indicates that the first iteration of StartCollect() has executed completely
	Counters []PdhCounter // Contains all PdhCounter's to be collected
	Done 	 chan struct{} // When this channel is closed, the collected Counters are unregistered from Prometheus and collection is stopped
	Host     string // Defines the host to collect Counters from
	Interval time.Duration // Defines the interval at which collection of Counters should be done
	PdhQHandle win.PDH_HQUERY // A handle to the PDH Query used for collecting Counters
	PdhCHandles map[string]*PdhCHandle // A handle to each PDH Counter
	PromCollectors map[string]prometheus.Gauge // Contains a reference to all prometheus collectors that have been created
	PromWaitGroup sync.WaitGroup // This is used to track if PromCollectors still contains active collectors
}

// PdhCHandle links a PDH handle to the consecutive number of times it has been collected unsuccessfully
type PdhCHandle struct {
	handle *win.PDH_HCOUNTER
	collectionFailures int
}

// StopCollect shuts down the collection that was started by StartCollect()
// and waits for all prometheus collectors to be unregistered.
func (p *PdhCounterSet) StopCollect() {
	// stop the old collection set
	close(p.Done)

	// Wait until all Prometheus Collectors have been unregistered to prevent clashing with registration of the new Collectors
	p.PromWaitGroup.Wait()
}

// StartCollect will start the collection for the defined Host and Counters in p
func (p *PdhCounterSet) StartCollect() error {
	defer p.UnregisterPrometheusCollectors()

	log.WithFields(log.Fields{
		"host": p.Host,
	}).Info("start StartCollect()")

	// Initialize basics of prometheus
	p.PromCollectors = map[string]prometheus.Gauge{}
	p.PromWaitGroup = sync.WaitGroup{}

	// Add a collector to track how many pdh counters fail to collect
	g := prometheus.GaugeOpts{
		ConstLabels:prometheus.Labels{"hostname":p.Host},
		Help: "The number of counters that failed to initialize",
		Name: "failed_collectors",
		Namespace:"winpdh",
	}
	if err := p.AddPrometheusCollector("FailedCollectors", g); err != nil {
		log.WithFields(log.Fields{
			"host": p.Host,
		}).Errorf("failed to add 'FailedCollectors' prometheus collector -> %s", err)
		return err
	}

	p.PdhCHandles = map[string]*PdhCHandle{}

	ret := win.PdhOpenQuery(0, 0, &p.PdhQHandle)
	if ret != win.ERROR_SUCCESS {
		log.WithFields(log.Fields{
			"host": p.Host,
			"PDHError": fmt.Sprintf("%x",ret),
		}).Error("failed PdhOpenQuery")
	} else {
		for _, c := range p.Counters {
			counter := fmt.Sprintf("\\\\%s%s", p.Host, c.Path)
			var c win.PDH_HCOUNTER
			ret = win.PdhValidatePath(counter)
			if ret == win.PDH_CSTATUS_BAD_COUNTERNAME {
				log.WithFields(log.Fields{
					"host": p.Host,
					"counter": counter,
					"PDHError": fmt.Sprintf("%x",ret),
				}).Error("failed PdhValidatePath")
				p.PromCollectors["FailedCollectors"].Add(1)
				continue
			}

			ret = win.PdhAddEnglishCounter(p.PdhQHandle, counter, 0, &c)
			if ret != win.ERROR_SUCCESS {
				if ret != win.PDH_CSTATUS_NO_OBJECT {
					log.WithFields(log.Fields{
						"counter": counter,
						"host": p.Host,
						"PDHError": fmt.Sprintf("%x",ret),
					}).Error("failed PdhAddEnglishCounter")
				} else {
					log.WithFields(log.Fields{
						"counter": counter,
						"host": p.Host,
						"PDHError": fmt.Sprintf("%x",ret),
					}).Warn("failed PdhAddEnglishCounter, most likely because the counter doesn't exist.")
				}
				p.PromCollectors["FailedCollectors"].Add(1)
				continue
			}

			p.PdhCHandles[counter] = &PdhCHandle{handle: &c}
		}

		ret = win.PdhCollectQueryData(p.PdhQHandle)
		if ret != win.ERROR_SUCCESS {
			// TODO: should I implement a custom error type here?
			return errors.New(fmt.Sprintf("failed PdhCollectQueryData with PDH error code: %x", ret))
		} else {
			loop:
			for {
				ret := win.PdhCollectQueryData(p.PdhQHandle)
				if ret == win.ERROR_SUCCESS {
					for k, v := range p.PdhCHandles {
						var bufSize uint32
						var bufCount uint32
						var size = uint32(unsafe.Sizeof(win.PDH_FMT_COUNTERVALUE_ITEM_DOUBLE{}))
						var emptyBuf [1]win.PDH_FMT_COUNTERVALUE_ITEM_DOUBLE // need at least 1 addressable null ptr.

						ret = win.PdhGetFormattedCounterArrayDouble(*v.handle, &bufSize, &bufCount, &emptyBuf[0])
						if ret == win.PDH_MORE_DATA {
							filledBuf := make([]win.PDH_FMT_COUNTERVALUE_ITEM_DOUBLE, bufCount*size)
							ret = win.PdhGetFormattedCounterArrayDouble(*v.handle, &bufSize, &bufCount, &filledBuf[0])
							if ret == win.ERROR_SUCCESS {
								for i := 0; i < int(bufCount); i++ {
									c := filledBuf[i]
									s := win.UTF16PtrToString(c.SzName)

									if val, ok := p.PromCollectors[k+s]; ok {
										val.Set(c.FmtValue.DoubleValue)
										v.collectionFailures = 0
									} else {
										if g, err := counterToPrometheusGauge(k, s); err == nil {
											if err := p.AddPrometheusCollector(k+s, g); err != nil {
												if e, ok := err.(prometheus.AlreadyRegisteredError); ok {
													log.WithFields(log.Fields{
														"counter": k,
														"PDHInstance": s,
														"host": p.Host,
														"error": e,
													}).Warnf("Collector already registered with prometheus")
												} else {
													log.WithFields(log.Fields{
														"counter": k,
														"PDHInstance": s,
														"host": p.Host,
														"error": err,
													}).Error("failed to register with prometheus")
													close(p.Done)
													return err
												}
											} else {
												log.WithFields(log.Fields{
													"counter": k,
													"PDHInstance": s,
													"host": p.Host,
												}).Debug("Collector registered with prometheus")
											}
										} else {
											log.WithFields(log.Fields{
												"counter": k,
												"host": p.Host,
												"error": err,
											}).Error("failed counterToPrometheusGauge")
										}
									}
								}
							} else {
								log.WithFields(log.Fields{
									"counter": k,
									"host": p.Host,
									"PDHError": fmt.Sprintf("%x",ret),
								}).Error("failed PdhGetFormattedCounterArrayDouble")
								p.handleCollectionFailure(k, v, ret)
							}
						} else {
							log.WithFields(log.Fields{
								"counter": k,
								"host": p.Host,
							}).Warn("No data exists for counter.")
							p.handleCollectionFailure(k, v, ret)
						}
					}
				}

				if !p.completedInitialization {
					p.completedInitialization = true
					log.WithFields(log.Fields{
						"host": p.Host,
					}).Info("completed StartCollect() initialization")
				} else {
					log.WithFields(log.Fields{
						"host": p.Host,
					}).Debug("completed StartCollect() iteration")
				}

				select{
				case <- p.Done:
					log.WithFields(log.Fields{
						"host": p.Host,
					}).Info("instance Done channel was closed")
					break loop // must specify name of loop or else it will just break out of select{}
				case <- time.After(p.Interval):
					// do nothing
				}
			}
		}
	}

	return nil
}

// AddPrometheusCollector adds a new gauge into PromCollectors and updates the number in PromWaitGroup
func (p *PdhCounterSet) AddPrometheusCollector(key string, g prometheus.GaugeOpts) error {
	p.PromCollectors[key] = prometheus.NewGauge(g)
	if err := prometheus.Register(p.PromCollectors[key]); err != nil {
		return err
	} else {
		p.PromWaitGroup.Add(1)
		return nil
	}
}

// UnregisterPrometheusCollectors unregisters all prometheus collector instances in use by p
func (p *PdhCounterSet) UnregisterPrometheusCollectors() {
	for k, v := range p.PromCollectors {
		if b := prometheus.Unregister(v); !b {
			log.WithFields(log.Fields{
				"collector": k,
				"host": p.Host,
			}).Error("failed to unregister Prometheus Collector\n")
		} else {
			delete(p.PromCollectors, k)
			p.PromWaitGroup.Done()
			log.WithFields(log.Fields{
				"collector": k,
				"host": p.Host,
			}).Debug("unregistered Prometheus Collector")
		}
	}
}

// TestEquivalence will test if a is equivalent to p
func (p *PdhCounterSet) TestEquivalence(a *PdhCounterSet) bool {
	if p.Host != a.Host || p.Interval != a.Interval || len(p.Counters) != len(a.Counters) {
		return false
	}

	for i := range p.Counters {
		if !p.Counters[i].TestEquivalence(&a.Counters[i]) {
			return false
		}
	}

	return true
}

// handleCollectionFailure is used to calculate when a counter should be deemed as non-collectible.
func (p *PdhCounterSet) handleCollectionFailure(counter string, cHandle *PdhCHandle, ret uint32) {
	cHandle.collectionFailures++

	if cHandle.collectionFailures == 10 {
		p.PromCollectors["FailedCollectors"].Add(1)

		// stop collection of counter
		delete(p.PdhCHandles, counter)

		log.WithFields(log.Fields{
			"counter":  counter,
			"host":     p.Host,
			"PDHError": fmt.Sprintf("%x",ret),
		}).Info("Stopping collection of counter due to 10 consecutive failed attempts.")
	}
}

// counterToPrometheusGauge converts a windows performance counter string into
// a prometheus Gauge.
//
// According to https://prometheus.io/docs/concepts/data_model/
// 		- Prometheus Metric Names must match: [a-zA-Z_:][a-zA-Z0-9_:]*
//		- Prometheus Label Restrictions:
// 			- Label names must match: [a-zA-Z_][a-zA-Z0-9_]*
//			- Label values: may contain any Unicode characters
//
// Additional Prometheus Metric/Label naming conventions: https://prometheus.io/docs/practices/naming/
func counterToPrometheusGauge(counter, instance string) (prometheus.GaugeOpts, error) {
	fields := strings.Split(counter, "\\")
	var hostname string
	var catIndex int
	var valIndex int
	var category string

	// If the string contains a hostname
	if len(fields) == 5 {
		hostname = fields[2]
		catIndex = 3
		valIndex = 4
	} else if len(fields) == 3 {
		hostname = "localhost"
		catIndex = 1
		valIndex = 2
	} else {
		return prometheus.GaugeOpts{}, errors.New("Unknown number of fields in counter: " + counter)
	}

	if strings.Contains(fields[catIndex], "(") {
		catFields := strings.Split(fields[catIndex], "(")
		category = catFields[0]
		i := strings.TrimSuffix(catFields[1], ")")
		if i != "*" {
			instance = i
		}
	} else {
		category = fields[catIndex]
	}

	// Replace known runes that occur in winpdh
	r := strings.NewReplacer(
		".", "_",
		"-", "_",
		" ", "_",
		"/","_",
		"%", "percent",
	)
	counterName := r.Replace(fields[valIndex])
	instance = r.Replace(instance)

	// Use this regex to replace any invalid characters that weren't accounted for already
	reg, err := regexp.Compile("[^a-zA-Z0-9_:]")
	if err != nil {
		return prometheus.GaugeOpts{}, err
	}

	category = string(reg.ReplaceAll([]byte(category),[]byte("")))
	instance = string(reg.ReplaceAll([]byte(instance),[]byte("")))

	return prometheus.GaugeOpts{
		ConstLabels: prometheus.Labels{"hostname": hostname, "pdhcategory": category, "pdhinstance": instance},
		Help: "windows performance counter",
		Name: string(reg.ReplaceAll([]byte(counterName),[]byte(""))),
		Namespace:"winpdh",
	}, nil
}