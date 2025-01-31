/*

Copyright 2019 The Vitess Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package mysql

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"vitess.io/vitess/go/mysql/capabilities"
	"vitess.io/vitess/go/mysql/replication"
	"vitess.io/vitess/go/mysql/sqlerror"
	"vitess.io/vitess/go/vt/vterrors"

	vtrpcpb "vitess.io/vitess/go/vt/proto/vtrpc"
)

// mariadbFlavor implements the Flavor interface for MariaDB.
type mariadbFlavor struct {
	serverVersion string
}
type mariadbFlavor101 struct {
	mariadbFlavor
}
type mariadbFlavor102 struct {
	mariadbFlavor
}

var _ flavor = (*mariadbFlavor101)(nil)
var _ flavor = (*mariadbFlavor102)(nil)

// primaryGTIDSet is part of the Flavor interface.
func (mariadbFlavor) primaryGTIDSet(c *Conn) (replication.GTIDSet, error) {
	qr, err := c.ExecuteFetch("SELECT @@GLOBAL.gtid_binlog_pos", 1, false)
	if err != nil {
		return nil, err
	}
	if len(qr.Rows) != 1 || len(qr.Rows[0]) != 1 {
		return nil, vterrors.Errorf(vtrpcpb.Code_INTERNAL, "unexpected result format for gtid_binlog_pos: %#v", qr)
	}

	return replication.ParseMariadbGTIDSet(qr.Rows[0][0].ToString())
}

// purgedGTIDSet is part of the Flavor interface.
func (mariadbFlavor) purgedGTIDSet(c *Conn) (replication.GTIDSet, error) {
	return nil, nil
}

// serverUUID is part of the Flavor interface.
func (mariadbFlavor) serverUUID(c *Conn) (string, error) {
	return "", nil
}

// gtidMode is part of the Flavor interface.
func (mariadbFlavor) gtidMode(c *Conn) (string, error) {
	return "", nil
}

func (mariadbFlavor) startReplicationUntilAfter(pos replication.Position) string {
	return fmt.Sprintf("START SLAVE UNTIL master_gtid_pos = \"%s\"", pos)
}

func (mariadbFlavor) startSQLThreadUntilAfter(pos replication.Position) string {
	return fmt.Sprintf("START SLAVE SQL_THREAD UNTIL master_gtid_pos = \"%s\"", pos)
}

func (mariadbFlavor) startReplicationCommand() string {
	return "START SLAVE"
}

func (mariadbFlavor) restartReplicationCommands() []string {
	return []string{
		"STOP SLAVE",
		"RESET SLAVE",
		"START SLAVE",
	}
}

func (mariadbFlavor) stopReplicationCommand() string {
	return "STOP SLAVE"
}

func (mariadbFlavor) resetReplicationCommand() string {
	return "RESET SLAVE ALL"
}

func (mariadbFlavor) stopIOThreadCommand() string {
	return "STOP SLAVE IO_THREAD"
}

func (mariadbFlavor) stopSQLThreadCommand() string {
	return "STOP SLAVE SQL_THREAD"
}

func (mariadbFlavor) startSQLThreadCommand() string {
	return "START SLAVE SQL_THREAD"
}

// sendBinlogDumpCommand is part of the Flavor interface.
func (mariadbFlavor) sendBinlogDumpCommand(c *Conn, serverID uint32, binlogFilename string, startPos replication.Position) error {
	// Tell the server that we understand GTIDs by setting
	// mariadb_slave_capability to MARIA_SLAVE_CAPABILITY_GTID = 4 (MariaDB >= 10.0.1).
	if _, err := c.ExecuteFetch("SET @mariadb_slave_capability=4", 0, false); err != nil {
		return vterrors.Wrapf(err, "failed to set @mariadb_slave_capability=4")
	}

	// Set the slave_connect_state variable before issuing COM_BINLOG_DUMP
	// to provide the start position in GTID form.
	query := fmt.Sprintf("SET @slave_connect_state='%s'", startPos)
	if _, err := c.ExecuteFetch(query, 0, false); err != nil {
		return vterrors.Wrapf(err, "failed to set @slave_connect_state='%s'", startPos)
	}

	// Real replicas set this upon connecting if their gtid_strict_mode option
	// was enabled. We always use gtid_strict_mode because we need it to
	// make our internal GTID comparisons safe.
	if _, err := c.ExecuteFetch("SET @slave_gtid_strict_mode=1", 0, false); err != nil {
		return vterrors.Wrapf(err, "failed to set @slave_gtid_strict_mode=1")
	}

	// Since we use @slave_connect_state, the file and position here are
	// ignored.
	return c.WriteComBinlogDump(serverID, "", 0, 0)
}

// resetReplicationCommands is part of the Flavor interface.
func (mariadbFlavor) resetReplicationCommands(c *Conn) []string {
	resetCommands := []string{
		"STOP SLAVE",
		"RESET SLAVE ALL", // "ALL" makes it forget source host:port.
		"RESET MASTER",
		"SET GLOBAL gtid_slave_pos = ''",
	}
	semisyncType, _ := c.SemiSyncExtensionLoaded()
	if semisyncType == SemiSyncTypeMaster {
		resetCommands = append(resetCommands, "SET GLOBAL rpl_semi_sync_master_enabled = false, GLOBAL rpl_semi_sync_slave_enabled = false") // semi-sync will be enabled if needed when replica is started.
	}
	return resetCommands
}

// resetReplicationParametersCommands is part of the Flavor interface.
func (mariadbFlavor) resetReplicationParametersCommands(c *Conn) []string {
	resetCommands := []string{
		"RESET SLAVE ALL", // "ALL" makes it forget source host:port.
	}
	return resetCommands
}

// setReplicationPositionCommands is part of the Flavor interface.
func (mariadbFlavor) setReplicationPositionCommands(pos replication.Position) []string {
	return []string{
		// RESET MASTER will clear out gtid_binlog_pos,
		// which then guarantees that gtid_current_pos = gtid_slave_pos,
		// since gtid_current_pos = MAX(gtid_binlog_pos,gtid_slave_pos).
		// This also emptys the binlogs, which allows us to set
		// gtid_binlog_state.
		"RESET MASTER",
		// Set gtid_slave_pos to tell the replica where to start
		// replicating.
		fmt.Sprintf("SET GLOBAL gtid_slave_pos = '%s'", pos),
		// Set gtid_binlog_state so that if this server later becomes the
		// primary, it will know that it has seen everything up to and
		// including 'pos'. Otherwise, if another replica asks this
		// server to replicate starting at exactly 'pos', this server
		// will throw an error when in gtid_strict_mode, since it
		// doesn't see 'pos' in its binlog - it only has everything
		// AFTER.
		fmt.Sprintf("SET GLOBAL gtid_binlog_state = '%s'", pos),
	}
}

func (mariadbFlavor) setReplicationSourceCommand(params *ConnParams, host string, port int32, connectRetry int) string {
	args := []string{
		fmt.Sprintf("MASTER_HOST = '%s'", host),
		fmt.Sprintf("MASTER_PORT = %d", port),
		fmt.Sprintf("MASTER_USER = '%s'", params.Uname),
		fmt.Sprintf("MASTER_PASSWORD = '%s'", params.Pass),
		fmt.Sprintf("MASTER_CONNECT_RETRY = %d", connectRetry),
	}
	if params.SslEnabled() {
		args = append(args, "MASTER_SSL = 1")
	}
	if params.SslCa != "" {
		args = append(args, fmt.Sprintf("MASTER_SSL_CA = '%s'", params.SslCa))
	}
	if params.SslCaPath != "" {
		args = append(args, fmt.Sprintf("MASTER_SSL_CAPATH = '%s'", params.SslCaPath))
	}
	if params.SslCert != "" {
		args = append(args, fmt.Sprintf("MASTER_SSL_CERT = '%s'", params.SslCert))
	}
	if params.SslKey != "" {
		args = append(args, fmt.Sprintf("MASTER_SSL_KEY = '%s'", params.SslKey))
	}
	args = append(args, "MASTER_USE_GTID = current_pos")
	return "CHANGE MASTER TO\n  " + strings.Join(args, ",\n  ")
}

// status is part of the Flavor interface.
func (mariadbFlavor) status(c *Conn) (replication.ReplicationStatus, error) {
	qr, err := c.ExecuteFetch("SHOW ALL SLAVES STATUS", 100, true /* wantfields */)
	if err != nil {
		return replication.ReplicationStatus{}, err
	}
	if len(qr.Rows) == 0 {
		// The query returned no data, meaning the server
		// is not configured as a replica.
		return replication.ReplicationStatus{}, ErrNotReplica
	}

	resultMap, err := resultToMap(qr)
	if err != nil {
		return replication.ReplicationStatus{}, err
	}

	return replication.ParseMariadbReplicationStatus(resultMap)
}

// primaryStatus is part of the Flavor interface.
func (m mariadbFlavor) primaryStatus(c *Conn) (replication.PrimaryStatus, error) {
	qr, err := c.ExecuteFetch("SHOW MASTER STATUS", 100, true /* wantfields */)
	if err != nil {
		return replication.PrimaryStatus{}, err
	}
	if len(qr.Rows) == 0 {
		// The query returned no data. We don't know how this could happen.
		return replication.PrimaryStatus{}, ErrNoPrimaryStatus
	}

	resultMap, err := resultToMap(qr)
	if err != nil {
		return replication.PrimaryStatus{}, err
	}

	status := replication.ParsePrimaryStatus(resultMap)
	status.Position.GTIDSet, err = m.primaryGTIDSet(c)
	return status, err
}

// waitUntilPosition is part of the Flavor interface.
//
// Note: Unlike MASTER_POS_WAIT(), MASTER_GTID_WAIT() will continue waiting even
// if the sql thread stops. If that is a problem, we'll have to change this.
func (mariadbFlavor) waitUntilPosition(ctx context.Context, c *Conn, pos replication.Position) error {
	// Omit the timeout to wait indefinitely. In MariaDB, a timeout of 0 means
	// return immediately.
	query := fmt.Sprintf("SELECT MASTER_GTID_WAIT('%s')", pos)
	if deadline, ok := ctx.Deadline(); ok {
		timeout := time.Until(deadline)
		if timeout <= 0 {
			return vterrors.Errorf(vtrpcpb.Code_DEADLINE_EXCEEDED, "timed out waiting for position %v", pos)
		}
		query = fmt.Sprintf("SELECT MASTER_GTID_WAIT('%s', %.6f)", pos, timeout.Seconds())
	}

	result, err := c.ExecuteFetch(query, 1, false)
	if err != nil {
		return err
	}

	// For MASTER_GTID_WAIT(), if the wait completes without a timeout 0 is
	// returned and -1 if there was a timeout.
	if len(result.Rows) != 1 || len(result.Rows[0]) != 1 {
		return vterrors.Errorf(vtrpcpb.Code_INTERNAL, "invalid results: %#v", result)
	}
	val := result.Rows[0][0]
	state, err := val.ToInt64()
	if err != nil {
		return vterrors.Errorf(vtrpcpb.Code_INTERNAL, "invalid result of %#v", val)
	}
	switch state {
	case 0:
		return nil
	case -1:
		return vterrors.Errorf(vtrpcpb.Code_DEADLINE_EXCEEDED, "timed out waiting for position %v", pos)
	default:
		return vterrors.Errorf(vtrpcpb.Code_INTERNAL, "invalid result of %d", state)
	}
}

// readBinlogEvent is part of the Flavor interface.
func (mariadbFlavor) readBinlogEvent(c *Conn) (BinlogEvent, error) {
	result, err := c.ReadPacket()
	if err != nil {
		return nil, err
	}
	switch result[0] {
	case EOFPacket:
		return nil, sqlerror.NewSQLError(sqlerror.CRServerLost, sqlerror.SSUnknownSQLState, "%v", io.EOF)
	case ErrPacket:
		return nil, ParseErrorPacket(result)
	}
	buf, semiSyncAckRequested, err := c.AnalyzeSemiSyncAckRequest(result[1:])
	if err != nil {
		return nil, err
	}
	ev := NewMariadbBinlogEventWithSemiSyncInfo(buf, semiSyncAckRequested)
	return ev, nil
}

// supportsCapability is part of the Flavor interface.
func (mariadbFlavor) supportsCapability(capability capabilities.FlavorCapability) (bool, error) {
	switch capability {
	default:
		return false, nil
	}
}

func (mariadbFlavor) catchupToGTIDCommands(_ *ConnParams, _ replication.Position) []string {
	return []string{"unsupported"}
}

func (mariadbFlavor) binlogReplicatedUpdates() string {
	return "@@global.log_slave_updates"
}
