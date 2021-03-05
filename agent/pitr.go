package agent

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/mongo"

	"github.com/percona/percona-backup-mongodb/pbm"
	"github.com/percona/percona-backup-mongodb/pbm/backup"
	"github.com/percona/percona-backup-mongodb/pbm/pitr"
	"github.com/percona/percona-backup-mongodb/pbm/restore"
)

type currentPitr struct {
	wakeup chan struct{} // to wake up a slicer on demand (not to wait for the tick)
	cancel context.CancelFunc
}

func (a *Agent) setPitr(p *currentPitr) (changed bool) {
	a.mx.Lock()
	defer a.mx.Unlock()
	if a.pitrjob != nil {
		return false
	}

	a.pitrjob = p
	return true
}

func (a *Agent) unsetPitr() {
	a.mx.Lock()
	a.pitrjob = nil
	a.mx.Unlock()
}

func (a *Agent) ispitr() bool {
	a.mx.Lock()
	defer a.mx.Unlock()
	return a.pitrjob != nil
}

func (a *Agent) cancelPitr() {
	a.mx.Lock()
	defer a.mx.Unlock()
	if a.pitrjob == nil {
		return
	}

	a.pitrjob.cancel()
}

func (a *Agent) wakeupPitr() {
	a.mx.Lock()
	defer a.mx.Unlock()
	if a.pitrjob == nil {
		return
	}

	a.pitrjob.wakeup <- struct{}{}
}

func (a *Agent) ResyncPITR() {
	a.mx.Lock()
	defer a.mx.Unlock()

	a.pitrsync <- struct{}{}

	<-a.pitrsync
}

const pitrCheckPeriod = time.Second * 15

// PITR starts PITR prcessing routine
func (a *Agent) PITR() {
	a.log.Printf("starting PITR routine")

	logError := func(err error) {
		// we need epoch just to log pitr err with an extra context
		// so not much care if we get it or not
		ep, _ := a.pbm.GetEpoch()
		a.log.Error(string(pbm.CmdPITR), "", "", ep.TS(), "init: %v", err)
	}

	a.pitrsync = make(chan struct{})

	ticker := time.NewTicker(pitrCheckPeriod)

	for {
		select {
		case <-ticker.C:
			err := a.pitr()
			if err != nil {
				logError(err)

				// penalty to the failed node so healthy nodes would have priority on next try
				time.Sleep(pitrCheckPeriod)
			}

		case <-a.pitrsync:
			err := a.pitr()
			if err != nil { // TODO: send error to the caller through channel? retry a.pitr() call after delay?
				logError(err)
			}

			a.pitrsync <- struct{}{}

		default:

		}
	}
}

func (a *Agent) pitr() (err error) {
	if atomic.LoadUint32(&a.intent) == intentBackup {
		return nil
	}

	on, err := a.pbm.IsPITR()
	if err != nil {
		return errors.Wrap(err, "check if on")
	}
	if !on {
		a.cancelPitr()
		return nil
	}

	// we already do the job
	if on && a.ispitr() {
		return nil
	}

	// just a check before a real locking
	// just trying to avoid redundant heavy operations
	moveOn, err := a.pitrLockCheck()
	if err != nil {
		return errors.Wrap(err, "check if already run")
	}

	if !moveOn {
		return nil
	}

	// shoud be after the lock pre-check
	//
	// if node failing, then some other agent with healthy node will hopefully catchup
	// so this code won't be reached and will not pollute log with "pitr" errors while
	// the other node does successfully slice
	ninf, err := a.node.GetInfo()
	if err != nil {
		return errors.Wrap(err, "get node info")
	}
	q, err := backup.NodeSuits(a.node, ninf)
	if err != nil {
		return errors.Wrap(err, "node check")
	}

	// node is not suitable for doing backup
	if !q {
		return nil
	}

	ibcp := pitr.NewBackup(a.node.RS(), a.pbm, a.node)

	err = ibcp.Catchup()
	if err != nil {
		return errors.Wrap(err, "defining starting point for the backup")
	}

	ep, err := a.pbm.GetEpoch()
	if err != nil {
		return errors.Wrap(err, "get epoch")
	}
	l := a.log.NewEvent(string(pbm.CmdPITR), "", "", ep.TS())

	stg, err := a.pbm.GetStorage(l)
	if err != nil {
		return errors.Wrap(err, "unable to get storage configuration")
	}

	epts := ep.TS()
	lock := a.pbm.NewLock(pbm.LockHeader{
		Replset: a.node.RS(),
		Node:    a.node.Name(),
		Type:    pbm.CmdPITR,
		Epoch:   &epts,
	})

	got, err := a.aquireLock(lock)
	if err != nil {
		return errors.Wrap(err, "acquiring lock")
	}
	if !got {
		l.Debug("skip: lock not acquired")
		return nil
	}

	go func() {
		ctx, cancel := context.WithCancel(context.Background())
		w := make(chan struct{})

		a.setPitr(&currentPitr{
			cancel: cancel,
			wakeup: w,
		})

		streamErr := ibcp.Stream(ctx, ep, w, stg, pbm.CompressionTypeS2)
		if streamErr != nil {
			switch streamErr.(type) {
			case pitr.ErrOpMoved:
				l.Info("streaming oplog: %v", streamErr)
			default:
				l.Error("streaming oplog: %v", streamErr)
			}
		}

		if err := lock.Release(); err != nil {
			l.Error("release lock: %v", err)
		}

		// Penalty to the failed node so healthy nodes would have priority on next try.
		// But lock has to be released first. Otherwise, healthy nodes would wait for the lock release
		// and the penalty won't have any sense.
		if streamErr != nil {
			time.Sleep(pitrCheckPeriod * 2)
		}

		a.unsetPitr()
	}()

	return nil
}

func (a *Agent) pitrLockCheck() (moveOn bool, err error) {
	ts, err := a.pbm.ClusterTime()
	if err != nil {
		return false, errors.Wrap(err, "read cluster time")
	}

	tl, err := a.pbm.GetLockData(&pbm.LockHeader{Replset: a.node.RS()})
	if err != nil {
		// mongo.ErrNoDocuments means no lock and we're good to move on
		if err == mongo.ErrNoDocuments {
			return true, nil
		}

		return false, errors.Wrap(err, "get lock")
	}

	// stale lock means we should move on and clean it up during the lock.Aquire
	return tl.Heartbeat.T+pbm.StaleFrameSec < ts.T, nil
}

// PITRestore starts the point-in-time recovery
func (a *Agent) PITRestore(r pbm.PITRestoreCmd, opid pbm.OPID, ep pbm.Epoch) {
	l := a.log.NewEvent(string(pbm.CmdPITR), time.Unix(r.TS, 0).UTC().Format(time.RFC3339), opid.String(), ep.TS())

	nodeInfo, err := a.node.GetInfo()
	if err != nil {
		l.Error("get node info: %v", err)
		return
	}
	if !nodeInfo.IsPrimary {
		l.Info("Node in not suitable for restore")
		return
	}

	epts := ep.TS()
	lock := a.pbm.NewLock(pbm.LockHeader{
		Type:    pbm.CmdPITRestore,
		Replset: nodeInfo.SetName,
		Node:    nodeInfo.Me,
		OPID:    opid.String(),
		Epoch:   &epts,
	})

	got, err := a.aquireLock(lock)
	if err != nil {
		l.Error("acquiring lock: %v", err)
		return
	}
	if !got {
		l.Debug("skip: lock not acquired")
		l.Error("unbale to run the restore while another backup or restore process running")
		return
	}

	defer func() {
		err := lock.Release()
		if err != nil {
			l.Error("release lock: %v", err)
		}
	}()

	l.Info("recovery started")
	err = restore.New(a.pbm, a.node).PITR(r, opid, l)
	if err != nil {
		if errors.Is(err, restore.ErrNoDatForShard) {
			l.Info("no data for the shard in backup, skipping")
		} else {
			l.Error("restore: %v", err)
		}
		return
	}
	l.Info("recovery successfully finished")

	if nodeInfo.IsLeader() {
		epch, err := a.pbm.ResetEpoch()
		if err != nil {
			l.Error("reset epoch")
			return
		}

		l.Debug("epoch set to %v", epch)
	}
}
