// Copyright 2017 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package ddl

import (
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/coreos/etcd/clientv3"
	"github.com/juju/errors"
	"github.com/ngaut/log"
	"github.com/pingcap/tidb/terror"
	goctx "golang.org/x/net/context"
)

const (
	ddlAllSchemaVersions   = "/tidb/ddl/all_schema_versions"
	ddlGlobalSchemaVersion = "/tidb/ddl/global_schema_version"
	initialVersion         = "0"
	putKeyNoRetry          = 1
	keyOpDefaultRetryCnt   = 3
	putKeyRetryUnlimited   = math.MaxInt64
	keyOpDefaultTimeout    = 2 * time.Second
	putKeyRetryInterval    = 30 * time.Millisecond
	checkVersInterval      = 20 * time.Millisecond
)

// checkVersFirstWaitTime is used for testing.
var checkVersFirstWaitTime = 50 * time.Millisecond

// SchemaSyncer is used to synchronize schema version between the DDL worker leader and followers through etcd.
type SchemaSyncer interface {
	// Init sets the global schema version path to etcd if it isn't exist,
	// then watch this path, and initializes the self schema version to etcd.
	Init(ctx goctx.Context) error
	// UpdateSelfVersion updates the current version to the self path on etcd.
	UpdateSelfVersion(ctx goctx.Context, version int64) error
	// RemoveSelfVersionPath remove the self path from etcd.
	RemoveSelfVersionPath() error
	// OwnerUpdateGlobalVersion updates the latest version to the global path on etcd.
	OwnerUpdateGlobalVersion(ctx goctx.Context, version int64) error
	// GlobalVersionCh gets the chan for watching global version.
	GlobalVersionCh() clientv3.WatchChan
	// OwnerCheckAllVersions checks whether all followers' schema version are equal to
	// the latest schema version. If the result is false, wait for a while and check again util the processing time reach 2 * lease.
	OwnerCheckAllVersions(ctx goctx.Context, latestVer int64) error
}

type schemaVersionSyncer struct {
	selfSchemaVerPath string
	etcdCli           *clientv3.Client
	globalVerCh       clientv3.WatchChan
}

// NewSchemaSyncer creates a new SchemaSyncer.
func NewSchemaSyncer(etcdCli *clientv3.Client, id string) SchemaSyncer {
	return &schemaVersionSyncer{
		etcdCli:           etcdCli,
		selfSchemaVerPath: fmt.Sprintf("%s/%s", ddlAllSchemaVersions, id),
	}
}

func (s *schemaVersionSyncer) putKV(ctx goctx.Context, retryCnt int, key, val string) error {
	var err error
	for i := 0; i < retryCnt; i++ {
		select {
		case <-ctx.Done():
			return errors.Trace(ctx.Err())
		default:
		}

		childCtx, cancel := goctx.WithTimeout(ctx, keyOpDefaultTimeout)
		_, err = s.etcdCli.Put(childCtx, key, val)
		cancel()
		if err == nil {
			return nil
		}
		log.Warnf("[syncer] put schema version %s failed %v no.%d", val, err, i)
		time.Sleep(putKeyRetryInterval)
	}
	return errors.Trace(err)
}

// Init implements SchemaSyncer.Init interface.
func (s *schemaVersionSyncer) Init(ctx goctx.Context) error {
	_, err := s.etcdCli.Txn(ctx).
		If(clientv3.Compare(clientv3.CreateRevision(ddlGlobalSchemaVersion), "=", 0)).
		Then(clientv3.OpPut(ddlGlobalSchemaVersion, initialVersion)).
		Commit()
	if err != nil {
		return errors.Trace(err)
	}
	s.globalVerCh = s.etcdCli.Watch(ctx, ddlGlobalSchemaVersion)
	return s.putKV(ctx, keyOpDefaultRetryCnt, s.selfSchemaVerPath, initialVersion)
}

// GlobalVersionCh implements SchemaSyncer.GlobalVersionCh interface.
func (s *schemaVersionSyncer) GlobalVersionCh() clientv3.WatchChan {
	return s.globalVerCh
}

// UpdateSelfVersion implements SchemaSyncer.UpdateSelfVersion interface.
func (s *schemaVersionSyncer) UpdateSelfVersion(ctx goctx.Context, version int64) error {
	ver := strconv.FormatInt(version, 10)
	return s.putKV(ctx, putKeyNoRetry, s.selfSchemaVerPath, ver)
}

// OwnerUpdateGlobalVersion implements SchemaSyncer.OwnerUpdateGlobalVersion interface.
func (s *schemaVersionSyncer) OwnerUpdateGlobalVersion(ctx goctx.Context, version int64) error {
	ver := strconv.FormatInt(version, 10)
	return s.putKV(ctx, putKeyRetryUnlimited, ddlGlobalSchemaVersion, ver)
}

// RemoveSelfVersionPath implements SchemaSyncer.RemoveSelfVersionPath interface.
func (s *schemaVersionSyncer) RemoveSelfVersionPath() error {
	ctx := goctx.Background()
	var err error
	for i := 0; i < keyOpDefaultRetryCnt; i++ {
		childCtx, cancel := goctx.WithTimeout(ctx, keyOpDefaultTimeout)
		_, err = s.etcdCli.Delete(childCtx, s.selfSchemaVerPath)
		cancel()
		if err == nil {
			return nil
		}
		log.Warnf("remove schema version path %s failed %v no.%d", s.selfSchemaVerPath, err, i)
	}
	return errors.Trace(err)
}

func isContextFinished(err error) bool {
	if terror.ErrorEqual(err, goctx.Canceled) ||
		terror.ErrorEqual(err, goctx.DeadlineExceeded) {
		return true
	}
	return false
}

// OwnerCheckAllVersions implements SchemaSyncer.OwnerCheckAllVersions interface.
func (s *schemaVersionSyncer) OwnerCheckAllVersions(ctx goctx.Context, latestVer int64) error {
	time.Sleep(checkVersFirstWaitTime)
	updatedMap := make(map[string]struct{})
	for {
		select {
		case <-ctx.Done():
			return errors.Trace(ctx.Err())
		default:
		}

		resp, err := s.etcdCli.Get(ctx, ddlAllSchemaVersions, clientv3.WithPrefix())
		if isContextFinished(err) {
			return errors.Trace(err)
		}
		if err != nil {
			log.Infof("[syncer] check all versions failed %v", err)
			continue
		}

		succ := true
		for _, kv := range resp.Kvs {
			if _, ok := updatedMap[string(kv.Key)]; ok {
				continue
			}

			ver, err := strconv.Atoi(string(kv.Value))
			if err != nil {
				log.Infof("[syncer] check all versions, ddl %s convert %v to int failed %v", kv.Key, kv.Value, err)
				succ = false
				break
			}
			if int64(ver) != latestVer {
				log.Infof("[syncer] check all versions, ddl %s current ver %v, latest version %v", kv.Key, ver, latestVer)
				succ = false
				break
			}
			updatedMap[string(kv.Key)] = struct{}{}
		}
		if succ {
			return nil
		}
		time.Sleep(checkVersInterval)
	}
}
