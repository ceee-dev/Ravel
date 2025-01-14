package director

import (
	"context"
	"fmt"
	"github.com/Comcast/Ravel/pkg/bgp"
	"io/ioutil"
	"sync"
	"time"

	"github.com/Comcast/Ravel/pkg/iptables"
	"github.com/Comcast/Ravel/pkg/stats"
	"github.com/Comcast/Ravel/pkg/system"
	"github.com/Comcast/Ravel/pkg/watcher"
	"github.com/sirupsen/logrus"
	log "github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
)

const (
	colocationModeDisabled = "disabled"
	colocationModeIPTables = "iptables"
	colocationModeIPVS     = "ipvs"
)

// TODO: instant startup

// A director is the control flow for kube2ipvs. It can only be started once, and it can only be stopped once.
type Director interface {
	Start() error
	Stop() error
}

type director struct {
	sync.Mutex

	// start/stop and backpropagation of internal errors
	isStarted bool
	doneChan  chan struct{}
	err       error

	// declarative state - this is what ought to be configured
	nodeName string
	node     *corev1.Node
	// nodes    []*corev1.Node
	// config   *types.ClusterConfig

	// inbound data sources
	nodeChan chan []*corev1.Node
	// configChan chan *types.ClusterConfig
	ctxWatch context.Context
	cxlWatch context.CancelFunc

	reconfiguring bool
	// lastInboundUpdate time.Time
	// lastReconfigure time.Time

	watcher  *watcher.Watcher
	ipvs     *system.IPVS
	ip       *system.IP
	iptables *iptables.IPTables

	// cli flag default false
	doCleanup         bool
	colocationMode    string
	forcedReconfigure bool
	// ipvsWeightOverride bool

	// boilerplate.  when this context is canceled, the director must cease all activties
	ctx     context.Context
	logger  logrus.FieldLogger
	metrics *stats.WorkerStateMetrics
}

func NewDirector(ctx context.Context, nodeName, configKey string, cleanup bool, watcher *watcher.Watcher, ipvs *system.IPVS, ip *system.IP, ipt *iptables.IPTables, colocationMode string, forcedReconfigure bool) (Director, error) {
	d := &director{
		watcher:  watcher,
		ipvs:     ipvs,
		ip:       ip,
		nodeName: nodeName,

		iptables: ipt,

		doneChan: make(chan struct{}),
		nodeChan: make(chan []*corev1.Node, 1),
		// configChan: make(chan *types.ClusterConfig, 1),

		doCleanup:         cleanup,
		ctx:               ctx,
		logger:            logrus.StandardLogger(),
		metrics:           stats.NewWorkerStateMetrics(stats.KindIpvsMaster, configKey),
		colocationMode:    colocationMode,
		forcedReconfigure: forcedReconfigure,
	}

	return d, nil
}

func (d *director) Start() error {
	if d.isStarted {
		return fmt.Errorf("director: director has already been started. a director instance can only be started once")
	}
	if d.reconfiguring {
		return fmt.Errorf("director: unable to Start. reconfiguration already in progress")
	}
	d.setReconfiguring(true)
	defer func() { d.setReconfiguring(false) }()
	d.logger.Debugf("director: start called")

	// init
	d.isStarted = true
	d.doneChan = make(chan struct{})

	// set arp rules
	err := d.ip.SetARP()
	if err != nil {
		return fmt.Errorf("director: cleanup - failed to clear arp rules - %v", err)
	}

	if d.colocationMode != colocationModeIPTables {
		// cleanup any lingering iptables rules
		if err := d.iptables.Flush(); err != nil {
			return fmt.Errorf("director: cleanup - failed to flush iptables - %v", err)
		}
	}
	// If director is co-located with a realserver, the realserver
	// will deal with setting up new iptables rules

	// instantitate a watcher and load this watcher instance into self
	ctxWatch, cxlWatch := context.WithCancel(d.ctx)
	d.ctxWatch = ctxWatch
	d.cxlWatch = cxlWatch

	// register the watcher for both nodes and the configmap
	// d.watcher.Nodes(ctxWatch, "director-nodes", d.nodeChan)
	// d.watcher.ConfigMap(ctxWatch, "director-configmap", d.configChan)

	// perform periodic configuration activities
	go d.periodic()
	go d.watches()
	go d.arps()

	// notify d.nodeChan and d.configChan like registering watchers
	// with the watcher.Watcher used to do
	go d.causePeriodicWatcherSync()

	d.logger.Debugf("director: setup complete. director is running")
	return nil
}

// causePeriodicWatcherSync patches the existing director logic into the watcher by
// periodically sending the latest information from the watcher to the old notification
// channels for changes the director was built with.
func (d *director) causePeriodicWatcherSync() {
	t := time.NewTicker(time.Second * 3)
	defer t.Stop()
	for {
		log.Debugln("director: causePeriodicWatcherSync: sending", len(d.watcher.Nodes), "to d.nodeChan")
		d.nodeChan <- d.watcher.Nodes
		<-t.C
		// log.Debugln("director: causePeriodicWatcherSync: sending", len(d.watcher.ClusterConfig.Config), "to d.configChan")
		// // d.configChan <- d.watcher.ClusterConfig
		// <-t.C
	}
}

// cleanup sets the initial state of the ipvs director by removing any KUBE-IPVS rules
// from the service chain and by clearing any arp rules that were set by a realserver
// on the same node.
// This function cannot clean up interface configurations, as the interface configurations
// rely on the presence of a config.
func (d *director) cleanup(ctx context.Context) error {
	errs := []string{}
	if err := d.iptables.Flush(); err != nil {
		errs = append(errs, fmt.Sprintf("cleanup - failed to flush iptables - %v", err))
	}

	if err := d.ip.Teardown(ctx, d.watcher.ClusterConfig.Config, d.watcher.ClusterConfig.Config6); err != nil {
		errs = append(errs, fmt.Sprintf("cleanup - failed to remove ip addresses - %v", err))
	}

	if err := d.ipvs.Teardown(ctx); err != nil {
		errs = append(errs, fmt.Sprintf("cleanup - failed to remove existing ipvs config - %v", err))
	}

	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("director: %v", errs)
}

func (d *director) Stop() error {
	if d.reconfiguring {
		return fmt.Errorf("director: unable to Stop. reconfiguration already in progress")
	}
	d.setReconfiguring(true)
	defer func() { d.setReconfiguring(false) }()

	// kill the watcher
	d.cxlWatch()
	d.logger.Info("director: blocking until periodic tasks complete")
	select {
	case <-d.doneChan:
	case <-time.After(5000 * time.Millisecond):
	}

	// remove config VIP addresses from the compute interface
	ctxDestroy, cxl := context.WithTimeout(context.Background(), 5000*time.Millisecond)
	defer cxl()

	if d.doCleanup {
		err := d.cleanup(ctxDestroy)
		d.isStarted = false
		return err
	}

	d.isStarted = false
	return nil
}

func (d *director) Err() error {
	return d.err
}

func (d *director) watches() {
	// XXX This things needs to actually get the list of nodes when a node update occurs
	// XXX It also needs to get all of the endpoints
	// XXX this thing needs a nonblocking, continuous read on the nodes channel and a
	// way to quiesce reads from this channel into actual behaviors in the app...
	for {
		select {

		case nodes := <-d.nodeChan:
			// d.logger.Debugf("director: watches: ", len(nodes), "nodes received from d.nodeChan")
			// if types.NodesEqual(d.watcher.Nodes, nodes) {
			// 	d.logger.Debug("NODES ARE EQUAL")
			// 	d.metrics.NodeUpdate("noop")
			// 	continue
			// }
			// d.metrics.NodeUpdate("updated")
			// d.logger.Debugf("director: watches: ", len(nodes), "nodes set from d.nodeChan")
			// d.nodes = nodes

			for _, node := range nodes {
				if node.Name == d.nodeName {
					d.Lock()
					d.node = node
					d.Unlock()
				}
			}
			// d.lastInboundUpdate = time.Now()

		// case configs := <-d.configChan:
		// 	d.logger.Debugf("director: watches: recv on configs")
		// 	d.Lock()
		// 	d.config = configs
		// 	d.lastInboundUpdate = time.Now()
		// 	d.Unlock()
		// 	d.metrics.ConfigUpdate()

		// 	// Administrative
		case <-d.ctx.Done():
			d.logger.Debugf("director: parent context closed. exiting run loop")
			return
		case <-d.ctxWatch.Done():
			d.logger.Debugf("director: watch context closed. exiting run loop")
			return
		}

	}
}

func (d *director) arps() {
	arpInterval := 2000 * time.Millisecond
	gratuitousArp := time.NewTicker(arpInterval)
	defer gratuitousArp.Stop()

	d.logger.Infof("director: starting periodic ticker. arp interval %v", arpInterval)
	for {
		select {
		case <-gratuitousArp.C:
			// every five minutes or so, walk the whole set of VIPs and make the call to
			// gratuitous arp.
			if d.watcher.ClusterConfig == nil || d.watcher.Nodes == nil {
				d.logger.Debugf("director: configs are nil. skipping arp clear")
				continue
			}
			ips := []string{}
			d.Lock()
			for ip := range d.watcher.ClusterConfig.Config {
				ips = append(ips, string(ip))
			}
			d.Unlock()
			for _, ip := range ips {
				if err := d.ip.AdvertiseMacAddress(ip); err != nil {
					d.metrics.ArpingFailure(err)
					d.logger.Error(err)
				}
			}

		case <-d.ctx.Done():
			d.logger.Debugf("director: parent context closed. exiting run loop")
			return
		case <-d.ctxWatch.Done():
			d.logger.Debugf("director: watch context closed. exiting run loop")
			return
		}
	}
}

func (d *director) periodic() {
	// reconfig ipvs
	checkInterval := time.Second * 2
	t := time.NewTicker(checkInterval)
	d.logger.Infof("director: starting periodic ticker. config check %v", checkInterval)

	forcedReconfigureInterval := time.Second * 60
	forceReconfigure := time.NewTicker(forcedReconfigureInterval)

	defer t.Stop()
	defer forceReconfigure.Stop()

	for {
		select {
		case <-forceReconfigure.C:
			if d.watcher.ClusterConfig.Config == nil {
				log.Warningln("director: Force reconfiguration skipped because d.config is nil")
				continue
			}
			if d.watcher.Nodes == nil {
				log.Warningln("director: Force reconfiguration skipped because d.nodes is nil")
				continue
			}
			d.logger.Info("director: Force reconfiguration w/o parity check timer went off")
			d.reconfigure(true)

		case <-t.C: // periodically apply declared state

			// if d.lastReconfigure.Sub(d.lastInboundUpdate) > 0 {
			// 	// Last reconfigure happened after the last update from watcher
			// 	d.logger.Debugf("director: no changes to configs since last reconfiguration completed")
			// 	continue
			// }

			// d.metrics.QueueDepth(len(d.configChan))

			if d.watcher.ClusterConfig.Config == nil {
				d.logger.Debugf("director: configs are nil. skipping apply")
				continue
			}
			if d.watcher.Nodes == nil {
				d.logger.Debugf("director: nodes are nil. skipping apply")
				continue
			}

			d.reconfigure(false)

		case <-d.ctx.Done():
			d.logger.Debugf("director: parent context closed. exiting run loop")
			return
		case <-d.ctxWatch.Done():
			d.logger.Debugf("director: watch context closed. exiting run loop")
			d.doneChan <- struct{}{}
			return
		}
	}
}

func (d *director) reconfigure(force bool) {
	start := time.Now()
	d.logger.Infof("director: reconfiguring")
	if err := d.applyConf(force); err != nil {
		d.logger.Errorf("error applying configuration in director. %v", err)
		return
	}
	d.logger.Infof("director: reconfiguration completed successfully in %v", time.Since(start))
	// d.lastReconfigure = start
}

func (d *director) applyConf(force bool) error {
	// TODO: this thing could have gotten a new copy of nodes by the
	// time it did its thing. need to lock in the caller, capture
	// the current time, deepcopy the nodes/config, and pass them into this.
	d.logger.Debugf("director: applying configuration")
	start := time.Now()

	// compare configurations and apply them
	if force {
		d.logger.Info("director: configuration parity ignored")
	} else {
		addressesV4, addressesV6, err := d.ip.Get()
		if err != nil {
			log.Errorln("director: error creating interface:", err)
		}

		// splice together to compare against the internal state of configs
		// addresses is sorted within the CheckConfigParity function
		addresses := append(addressesV4, addressesV6...)

		same, err := d.ipvs.CheckConfigParity(d.watcher, d.watcher.ClusterConfig, addresses)
		if err != nil {
			d.metrics.Reconfigure("error", time.Since(start))
			return fmt.Errorf("director: unable to compare configurations with error %v", err)
		}
		if same {
			d.metrics.Reconfigure("noop", time.Since(start))
			d.logger.Info("director: configuration has parity")
			return nil
		}

		d.logger.Info("director: configuration parity mismatch")
	}

	// Manage VIP addresses
	err := d.setAddresses()
	if err != nil {
		d.metrics.Reconfigure("error", time.Since(start))
		return fmt.Errorf("director: unable to configure VIP addresses with error %v", err)
	}
	d.logger.Debugf("director: addresses set")

	// Manage iptables configuration
	// only execute with cli flag ipvs-colocation-mode=true
	// this indicates the director is in a non-isolated load balancer tier
	if d.colocationMode == colocationModeIPTables {
		err = d.setIPTables()
		if err != nil {
			d.metrics.Reconfigure("error", time.Since(start))
			return fmt.Errorf("director: unable to configure iptables with error %v", err)
		}
		d.logger.Debugf("director: iptables configured")
	}

	// Manage ipvsadm configuration
	err = d.ipvs.SetIPVS(d.watcher, d.watcher.ClusterConfig, d.logger, bgp.AddrKindIPV4)

	if err != nil {
		d.metrics.Reconfigure("error", time.Since(start))
		return fmt.Errorf("director: unable to configure ipvs with error %v", err)
	}
	d.logger.Debugf("director: ipvs configured")

	d.metrics.Reconfigure("complete", time.Since(start))
	return nil
}

func (d *director) setIPTables() error {

	d.logger.Debugf("director: capturing iptables rules")
	// fetch existing iptables rules
	existing, err := d.iptables.Save()
	if err != nil {
		return err
	}
	d.logger.Debugf("director: got %d existing rules", len(existing))

	d.logger.Debugf("director: generating iptables rules")
	// i need to determine what percentage of traffic should be sent to the master
	// for each namespace/service:port that is in the config, i need to know the proportion
	// of the whole that namespace/service:port represents
	generated, err := d.iptables.GenerateRulesForNodeClassic(d.watcher, d.node.Name, d.watcher.ClusterConfig, true)
	if err != nil {
		return err
	}
	d.logger.Debugf("director: got %d generated rules", len(generated))

	d.logger.Debugf("director: merging iptables rules")

	merged, _, err := d.iptables.Merge(generated, existing) // subset, all rules

	if err != nil {
		return err
	}

	d.logger.Debugf("director: got %d merged rules", len(merged))

	d.logger.Debugf("director: applying updated rules")
	err = d.iptables.Restore(merged)
	if err != nil {
		// set our failure gauge for iptables alertmanagers
		d.metrics.IptablesWriteFailure(1)
		// write erroneous rule set to file to capture later
		d.logger.Errorf("error applying rules. writing erroneous rule change to /tmp/director-ruleset-err for debugging")
		writeErr := ioutil.WriteFile("/tmp/director-ruleset-err", createErrorLog(err, iptables.BytesFromRules(merged)), 0644)
		if writeErr != nil {
			d.logger.Errorf("error writing to file; logging rules: %s", string(iptables.BytesFromRules(merged)))
		}

		return err
	}

	// set gauge to success
	d.metrics.IptablesWriteFailure(0)
	return nil
}

// func (d *director) configReady() bool {
// 	newConfig := false
// 	d.Lock()
// 	if d.newConfig {
// 		newConfig = true
// 		d.newConfig = false
// 	}
// 	d.Unlock()
// 	return newConfig
// }

func (d *director) setAddresses() error {
	// pull existing
	configuredV4, _, err := d.ip.Get()
	if err != nil {
		return err
	}

	// get desired VIP addresses
	desired := []string{}
	for ip := range d.watcher.ClusterConfig.Config {
		desired = append(desired, string(ip))
	}

	// XXX statsd
	removals, additions := d.ip.Compare4(configuredV4, desired)

	for _, addr := range removals {
		d.logger.WithFields(logrus.Fields{"device": "primary", "addr": addr, "action": "deleting"}).Info()
		err := d.ip.Del(addr)
		if err != nil {
			return err
		}
	}
	for _, addr := range additions {
		d.logger.WithFields(logrus.Fields{"device": "primary", "addr": addr, "action": "adding"}).Info()
		if err := d.ip.Add(addr); err != nil {
			log.Errorln("director: error adding adapter:", addr, err)
		}
		if err := d.ip.AdvertiseMacAddress(addr); err != nil {
			d.logger.Warnf("director: error setting gratuitous arp. this is most likely due to the VIP not being present on the interface. %s", err)
		}
	}

	// now iterate across configured and see if we have a non-standard MTU
	// setting it where applicable
	err = d.ip.SetMTU(d.watcher.ClusterConfig.MTUConfig, false)
	if err != nil {
		log.Errorln("director: error setting MTU on adapters:", err)
	}

	return nil
}

func (d *director) setReconfiguring(v bool) {
	d.Lock()
	d.reconfiguring = v
	d.Unlock()
}

func createErrorLog(err error, rules []byte) []byte {
	if err == nil {
		return rules
	}

	errBytes := []byte(fmt.Sprintf("ipvs restore error: %v\n", err.Error()))
	return append(errBytes, rules...)
}
