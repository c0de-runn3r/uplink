// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

package satellite

import (
	"context"

	"github.com/zeebo/errs"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"

	"storj.io/common/errs2"
	"storj.io/common/identity"
	"storj.io/common/pb"
	"storj.io/common/peertls/extensions"
	"storj.io/common/peertls/tlsopts"
	"storj.io/common/rpc"
	"storj.io/common/signing"
	"storj.io/common/storj"
	"storj.io/storj/private/version"
	version_checker "storj.io/storj/private/version/checker"
	"storj.io/storj/satellite/accounting"
	"storj.io/storj/satellite/accounting/rollup"
	"storj.io/storj/satellite/accounting/tally"
	"storj.io/storj/satellite/audit"
	"storj.io/storj/satellite/contact"
	"storj.io/storj/satellite/dbcleanup"
	"storj.io/storj/satellite/downtime"
	"storj.io/storj/satellite/gc"
	"storj.io/storj/satellite/gracefulexit"
	"storj.io/storj/satellite/metainfo"
	"storj.io/storj/satellite/metrics"
	"storj.io/storj/satellite/orders"
	"storj.io/storj/satellite/overlay"
	"storj.io/storj/satellite/payments"
	"storj.io/storj/satellite/payments/mockpayments"
	"storj.io/storj/satellite/payments/stripecoinpayments"
	"storj.io/storj/satellite/repair/checker"
	"storj.io/storj/satellite/repair/repairer"
)

// Core is the satellite core process that runs chores
//
// architecture: Peer
type Core struct {
	// core dependencies
	Log      *zap.Logger
	Identity *identity.FullIdentity
	DB       DB

	Dialer rpc.Dialer

	Version *version_checker.Service

	// services and endpoints
	Contact struct {
		Service *contact.Service
	}

	Overlay struct {
		DB      overlay.DB
		Service *overlay.Service
	}

	Metainfo struct {
		Database metainfo.PointerDB // TODO: move into pointerDB
		Service  *metainfo.Service
		Loop     *metainfo.Loop
	}

	Orders struct {
		Service *orders.Service
	}

	Repair struct {
		Checker  *checker.Checker
		Repairer *repairer.Service
	}
	Audit struct {
		Queue    *audit.Queue
		Worker   *audit.Worker
		Chore    *audit.Chore
		Verifier *audit.Verifier
		Reporter *audit.Reporter
	}

	GarbageCollection struct {
		Service *gc.Service
	}

	DBCleanup struct {
		Chore *dbcleanup.Chore
	}

	Accounting struct {
		Tally        *tally.Service
		Rollup       *rollup.Service
		ProjectUsage *accounting.Service
	}

	LiveAccounting struct {
		Cache accounting.Cache
	}

	Payments struct {
		Accounts payments.Accounts
		Chore    *stripecoinpayments.Chore
	}

	GracefulExit struct {
		Chore *gracefulexit.Chore
	}

	Metrics struct {
		Chore *metrics.Chore
	}

	DowntimeTracking struct {
		DetectionChore *downtime.DetectionChore
		Service        *downtime.Service
	}
}

// New creates a new satellite
func New(log *zap.Logger, full *identity.FullIdentity, db DB, pointerDB metainfo.PointerDB, revocationDB extensions.RevocationDB, liveAccounting accounting.Cache, versionInfo version.Info, config *Config) (*Core, error) {
	peer := &Core{
		Log:      log,
		Identity: full,
		DB:       db,
	}

	var err error

	{ // setup version control
		if !versionInfo.IsZero() {
			peer.Log.Sugar().Debugf("Binary Version: %s with CommitHash %s, built at %s as Release %v",
				versionInfo.Version.String(), versionInfo.CommitHash, versionInfo.Timestamp.String(), versionInfo.Release)
		}
		peer.Version = version_checker.NewService(log.Named("version"), config.Version, versionInfo, "Satellite")
	}

	{ // setup listener and server
		log.Debug("Starting listener and server")
		sc := config.Server

		tlsOptions, err := tlsopts.NewOptions(peer.Identity, sc.Config, revocationDB)
		if err != nil {
			return nil, errs.Combine(err, peer.Close())
		}

		peer.Dialer = rpc.NewDefaultDialer(tlsOptions)
	}

	{ // setup contact service
		pbVersion, err := versionInfo.Proto()
		if err != nil {
			return nil, errs.Combine(err, peer.Close())
		}

		self := &overlay.NodeDossier{
			Node: pb.Node{
				Id: peer.ID(),
				Address: &pb.NodeAddress{
					Address: config.Contact.ExternalAddress,
				},
			},
			Type:    pb.NodeType_SATELLITE,
			Version: *pbVersion,
		}
		peer.Contact.Service = contact.NewService(peer.Log.Named("contact:service"), self, peer.Overlay.Service, peer.DB.PeerIdentities(), peer.Dialer)
	}

	{ // setup overlay
		peer.Overlay.DB = overlay.NewCombinedCache(peer.DB.OverlayCache())
		peer.Overlay.Service = overlay.NewService(peer.Log.Named("overlay"), peer.Overlay.DB, config.Overlay)
	}

	{ // setup live accounting
		peer.LiveAccounting.Cache = liveAccounting
	}

	{ // setup accounting project usage
		peer.Accounting.ProjectUsage = accounting.NewService(
			peer.DB.ProjectAccounting(),
			peer.LiveAccounting.Cache,
			config.Rollup.MaxAlphaUsage,
		)
	}

	{ // setup orders
		peer.Orders.Service = orders.NewService(
			peer.Log.Named("orders:service"),
			signing.SignerFromFullIdentity(peer.Identity),
			peer.Overlay.Service,
			peer.DB.Orders(),
			config.Orders.Expiration,
			&pb.NodeAddress{
				Transport: pb.NodeTransport_TCP_TLS_GRPC,
				Address:   config.Contact.ExternalAddress,
			},
			config.Repairer.MaxExcessRateOptimalThreshold,
		)
	}

	{ // setup metainfo
		peer.Metainfo.Database = pointerDB // for logging: storelogger.New(peer.Log.Named("pdb"), db)
		peer.Metainfo.Service = metainfo.NewService(peer.Log.Named("metainfo:service"),
			peer.Metainfo.Database,
			peer.DB.Buckets(),
		)
		peer.Metainfo.Loop = metainfo.NewLoop(config.Metainfo.Loop, peer.Metainfo.Database)
	}

	{ // setup datarepair
		// TODO: simplify argument list somehow
		peer.Repair.Checker = checker.NewChecker(
			peer.Log.Named("checker"),
			peer.DB.RepairQueue(),
			peer.DB.Irreparable(),
			peer.Metainfo.Service,
			peer.Metainfo.Loop,
			peer.Overlay.Service,
			config.Checker)

		segmentRepairer := repairer.NewSegmentRepairer(
			log.Named("repairer"),
			peer.Metainfo.Service,
			peer.Orders.Service,
			peer.Overlay.Service,
			peer.Dialer,
			config.Repairer.Timeout,
			config.Repairer.MaxExcessRateOptimalThreshold,
			config.Checker.RepairOverride,
			config.Repairer.DownloadTimeout,
			signing.SigneeFromPeerIdentity(peer.Identity.PeerIdentity()),
		)

		peer.Repair.Repairer = repairer.NewService(
			peer.Log.Named("repairer"),
			peer.DB.RepairQueue(),
			&config.Repairer,
			segmentRepairer,
		)
	}

	{ // setup audit
		config := config.Audit

		peer.Audit.Queue = &audit.Queue{}

		peer.Audit.Verifier = audit.NewVerifier(log.Named("audit:verifier"),
			peer.Metainfo.Service,
			peer.Dialer,
			peer.Overlay.Service,
			peer.DB.Containment(),
			peer.Orders.Service,
			peer.Identity,
			config.MinBytesPerSecond,
			config.MinDownloadTimeout,
		)

		peer.Audit.Reporter = audit.NewReporter(log.Named("audit:reporter"),
			peer.Overlay.Service,
			peer.DB.Containment(),
			config.MaxRetriesStatDB,
			int32(config.MaxReverifyCount),
		)

		peer.Audit.Worker, err = audit.NewWorker(peer.Log.Named("audit worker"),
			peer.Audit.Queue,
			peer.Audit.Verifier,
			peer.Audit.Reporter,
			config,
		)

		if err != nil {
			return nil, errs.Combine(err, peer.Close())
		}

		peer.Audit.Chore = audit.NewChore(peer.Log.Named("audit chore"),
			peer.Audit.Queue,
			peer.Metainfo.Loop,
			config,
		)
	}

	{ // setup garbage collection
		peer.GarbageCollection.Service = gc.NewService(
			peer.Log.Named("garbage collection"),
			config.GarbageCollection,
			peer.Dialer,
			peer.Overlay.DB,
			peer.Metainfo.Loop,
		)
	}

	{ // setup db cleanup
		peer.DBCleanup.Chore = dbcleanup.NewChore(peer.Log.Named("dbcleanup"), peer.DB.Orders(), config.DBCleanup)
	}

	{ // setup accounting
		peer.Accounting.Tally = tally.New(peer.Log.Named("tally"), peer.DB.StoragenodeAccounting(), peer.DB.ProjectAccounting(), peer.LiveAccounting.Cache, peer.Metainfo.Loop, config.Tally.Interval)
		peer.Accounting.Rollup = rollup.New(peer.Log.Named("rollup"), peer.DB.StoragenodeAccounting(), config.Rollup.Interval, config.Rollup.DeleteTallies)
	}

	// TODO: remove in future, should be in API
	{ // setup payments
		pc := config.Payments

		switch pc.Provider {
		default:
			peer.Payments.Accounts = mockpayments.Accounts()
		case "stripecoinpayments":
			service := stripecoinpayments.NewService(
				peer.Log.Named("payments.stripe:service"),
				pc.StripeCoinPayments,
				peer.DB.StripeCoinPayments(),
				peer.DB.Console().Projects(),
				peer.DB.ProjectAccounting(),
				pc.PerObjectPrice,
				pc.EgressPrice,
				pc.TbhPrice)

			peer.Payments.Accounts = service.Accounts()

			peer.Payments.Chore = stripecoinpayments.NewChore(
				peer.Log.Named("payments.stripe:clearing"),
				service,
				pc.StripeCoinPayments.TransactionUpdateInterval,
				pc.StripeCoinPayments.AccountBalanceUpdateInterval,
				// TODO: uncomment when coupons will be finished.
				//pc.StripeCoinPayments.CouponUsageCycleInterval,
			)
		}
	}

	{ // setup graceful exit
		if config.GracefulExit.Enabled {
			peer.GracefulExit.Chore = gracefulexit.NewChore(peer.Log.Named("gracefulexit"), peer.DB.GracefulExit(), peer.Overlay.DB, peer.Metainfo.Loop, config.GracefulExit)
		} else {
			peer.Log.Named("gracefulexit").Info("disabled")
		}
	}

	{ // setup metrics service
		peer.Metrics.Chore = metrics.NewChore(
			peer.Log.Named("metrics"),
			config.Metrics,
			peer.Metainfo.Loop,
		)
	}

	{ // setup downtime tracking
		peer.DowntimeTracking.Service = downtime.NewService(peer.Log.Named("downtime"), peer.Overlay.Service, peer.Contact.Service)

		peer.DowntimeTracking.DetectionChore = downtime.NewDetectionChore(
			peer.Log.Named("downtime:detection"),
			config.Downtime,
			peer.Overlay.Service,
			peer.DowntimeTracking.Service,
			peer.DB.DowntimeTracking(),
		)
	}

	return peer, nil
}

// Run runs satellite until it's either closed or it errors.
func (peer *Core) Run(ctx context.Context) (err error) {
	defer mon.Task()(&ctx)(&err)

	group, ctx := errgroup.WithContext(ctx)

	group.Go(func() error {
		return errs2.IgnoreCanceled(peer.Metainfo.Loop.Run(ctx))
	})
	group.Go(func() error {
		return errs2.IgnoreCanceled(peer.Version.Run(ctx))
	})
	group.Go(func() error {
		return errs2.IgnoreCanceled(peer.Repair.Checker.Run(ctx))
	})
	group.Go(func() error {
		return errs2.IgnoreCanceled(peer.Repair.Repairer.Run(ctx))
	})
	group.Go(func() error {
		return errs2.IgnoreCanceled(peer.DBCleanup.Chore.Run(ctx))
	})
	group.Go(func() error {
		return errs2.IgnoreCanceled(peer.Accounting.Tally.Run(ctx))
	})
	group.Go(func() error {
		return errs2.IgnoreCanceled(peer.Accounting.Rollup.Run(ctx))
	})
	group.Go(func() error {
		return errs2.IgnoreCanceled(peer.Audit.Worker.Run(ctx))
	})
	group.Go(func() error {
		return errs2.IgnoreCanceled(peer.Audit.Chore.Run(ctx))
	})
	group.Go(func() error {
		return errs2.IgnoreCanceled(peer.GarbageCollection.Service.Run(ctx))
	})
	if peer.GracefulExit.Chore != nil {
		group.Go(func() error {
			return errs2.IgnoreCanceled(peer.GracefulExit.Chore.Run(ctx))
		})
	}
	group.Go(func() error {
		return errs2.IgnoreCanceled(peer.Metrics.Chore.Run(ctx))
	})
	if peer.Payments.Chore != nil {
		group.Go(func() error {
			return errs2.IgnoreCanceled(peer.Payments.Chore.Run(ctx))
		})
	}
	group.Go(func() error {
		return errs2.IgnoreCanceled(peer.DowntimeTracking.DetectionChore.Run(ctx))
	})

	return group.Wait()
}

// Close closes all the resources.
func (peer *Core) Close() error {
	var errlist errs.Group

	// TODO: ensure that Close can be called on nil-s that way this code won't need the checks.

	// close servers, to avoid new connections to closing subsystems
	if peer.DowntimeTracking.DetectionChore != nil {
		errlist.Add(peer.DowntimeTracking.DetectionChore.Close())
	}

	if peer.Metrics.Chore != nil {
		errlist.Add(peer.Metrics.Chore.Close())
	}

	if peer.GracefulExit.Chore != nil {
		errlist.Add(peer.GracefulExit.Chore.Close())
	}

	// close services in reverse initialization order

	if peer.Audit.Chore != nil {
		errlist.Add(peer.Audit.Chore.Close())
	}
	if peer.Audit.Worker != nil {
		errlist.Add(peer.Audit.Worker.Close())
	}

	if peer.Accounting.Rollup != nil {
		errlist.Add(peer.Accounting.Rollup.Close())
	}
	if peer.Accounting.Tally != nil {
		errlist.Add(peer.Accounting.Tally.Close())
	}

	if peer.DBCleanup.Chore != nil {
		errlist.Add(peer.DBCleanup.Chore.Close())
	}
	if peer.Repair.Repairer != nil {
		errlist.Add(peer.Repair.Repairer.Close())
	}
	if peer.Repair.Checker != nil {
		errlist.Add(peer.Repair.Checker.Close())
	}

	if peer.Overlay.Service != nil {
		errlist.Add(peer.Overlay.Service.Close())
	}
	if peer.Contact.Service != nil {
		errlist.Add(peer.Contact.Service.Close())
	}
	if peer.Metainfo.Loop != nil {
		errlist.Add(peer.Metainfo.Loop.Close())
	}

	return errlist.Err()
}

// ID returns the peer ID.
func (peer *Core) ID() storj.NodeID { return peer.Identity.ID }
