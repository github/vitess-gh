/*
   Copyright 2015 Shlomi Noach, courtesy Booking.com

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

package logic

import (
	"fmt"
	"strings"

	"vitess.io/vitess/go/vt/external/golib/sqlutils"
	"vitess.io/vitess/go/vt/log"
	"vitess.io/vitess/go/vt/vtorc/config"
	"vitess.io/vitess/go/vt/vtorc/db"
	"vitess.io/vitess/go/vt/vtorc/inst"
	"vitess.io/vitess/go/vt/vtorc/process"
	"vitess.io/vitess/go/vt/vtorc/util"
)

// AttemptFailureDetectionRegistration tries to add a failure-detection entry; if this fails that means the problem has already been detected
func AttemptFailureDetectionRegistration(analysisEntry *inst.ReplicationAnalysis) (registrationSuccessful bool, err error) {
	args := sqlutils.Args(
		analysisEntry.AnalyzedInstanceKey.Hostname,
		analysisEntry.AnalyzedInstanceKey.Port,
		process.ThisHostname,
		util.ProcessToken.Hash,
		string(analysisEntry.Analysis),
		analysisEntry.ClusterDetails.ClusterName,
		analysisEntry.CountReplicas,
		analysisEntry.IsActionableRecovery,
	)
	startActivePeriodHint := "now()"
	if analysisEntry.StartActivePeriod != "" {
		startActivePeriodHint = "?"
		args = append(args, analysisEntry.StartActivePeriod)
	}

	query := fmt.Sprintf(`
			insert ignore
				into topology_failure_detection (
					hostname,
					port,
					in_active_period,
					end_active_period_unixtime,
					processing_node_hostname,
					processcing_node_token,
					analysis,
					cluster_name,
					count_affected_replicas,
					is_actionable,
					start_active_period
				) values (
					?,
					?,
					1,
					0,
					?,
					?,
					?,
					?,
					?,
					?,
					%s
				)
			`, startActivePeriodHint)

	sqlResult, err := db.ExecVTOrc(query, args...)
	if err != nil {
		log.Error(err)
		return false, err
	}
	rows, err := sqlResult.RowsAffected()
	if err != nil {
		log.Error(err)
		return false, err
	}
	return (rows > 0), nil
}

// ClearActiveFailureDetections clears the "in_active_period" flag for old-enough detections, thereby allowing for
// further detections on cleared instances.
func ClearActiveFailureDetections() error {
	_, err := db.ExecVTOrc(`
			update topology_failure_detection set
				in_active_period = 0,
				end_active_period_unixtime = UNIX_TIMESTAMP()
			where
				in_active_period = 1
				AND start_active_period < NOW() - INTERVAL ? MINUTE
			`,
		config.FailureDetectionPeriodBlockMinutes,
	)
	if err != nil {
		log.Error(err)
	}
	return err
}

func writeTopologyRecovery(topologyRecovery *TopologyRecovery) (*TopologyRecovery, error) {
	analysisEntry := topologyRecovery.AnalysisEntry
	sqlResult, err := db.ExecVTOrc(`
			insert ignore
				into topology_recovery (
					recovery_id,
					uid,
					hostname,
					port,
					in_active_period,
					start_active_period,
					end_active_period_unixtime,
					processing_node_hostname,
					processcing_node_token,
					analysis,
					cluster_name,
					count_affected_replicas,
					last_detection_id
				) values (
					?,
					?,
					?,
					?,
					1,
					NOW(),
					0,
					?,
					?,
					?,
					?,
					?,
					(select ifnull(max(detection_id), 0) from topology_failure_detection where hostname=? and port=?)
				)
			`,
		sqlutils.NilIfZero(topologyRecovery.ID),
		topologyRecovery.UID,
		analysisEntry.AnalyzedInstanceKey.Hostname, analysisEntry.AnalyzedInstanceKey.Port,
		process.ThisHostname, util.ProcessToken.Hash,
		string(analysisEntry.Analysis),
		analysisEntry.ClusterDetails.ClusterName,
		analysisEntry.CountReplicas,
		analysisEntry.AnalyzedInstanceKey.Hostname, analysisEntry.AnalyzedInstanceKey.Port,
	)
	if err != nil {
		return nil, err
	}
	rows, err := sqlResult.RowsAffected()
	if err != nil {
		return nil, err
	}
	if rows == 0 {
		return nil, nil
	}
	lastInsertID, err := sqlResult.LastInsertId()
	if err != nil {
		return nil, err
	}
	topologyRecovery.ID = lastInsertID
	return topologyRecovery, nil
}

// AttemptRecoveryRegistration tries to add a recovery entry; if this fails that means recovery is already in place.
func AttemptRecoveryRegistration(analysisEntry *inst.ReplicationAnalysis, failIfFailedInstanceInActiveRecovery bool, failIfClusterInActiveRecovery bool) (*TopologyRecovery, error) {
	if failIfFailedInstanceInActiveRecovery {
		// Let's check if this instance has just been promoted recently and is still in active period.
		// If so, we reject recovery registration to avoid flapping.
		recoveries, err := ReadInActivePeriodSuccessorInstanceRecovery(&analysisEntry.AnalyzedInstanceKey)
		if err != nil {
			log.Error(err)
			return nil, err
		}
		if len(recoveries) > 0 {
			_ = RegisterBlockedRecoveries(analysisEntry, recoveries)
			errMsg := fmt.Sprintf("AttemptRecoveryRegistration: instance %+v has recently been promoted (by failover of %+v) and is in active period. It will not be failed over. You may acknowledge the failure on %+v (-c ack-instance-recoveries) to remove this blockage", analysisEntry.AnalyzedInstanceKey, recoveries[0].AnalysisEntry.AnalyzedInstanceKey, recoveries[0].AnalysisEntry.AnalyzedInstanceKey)
			log.Errorf(errMsg)
			return nil, fmt.Errorf(errMsg)
		}
	}
	if failIfClusterInActiveRecovery {
		// Let's check if this cluster has just experienced a failover and is still in active period.
		// If so, we reject recovery registration to avoid flapping.
		recoveries, err := ReadInActivePeriodClusterRecovery(analysisEntry.ClusterDetails.ClusterName)
		if err != nil {
			log.Error(err)
			return nil, err
		}
		if len(recoveries) > 0 {
			_ = RegisterBlockedRecoveries(analysisEntry, recoveries)
			errMsg := fmt.Sprintf("AttemptRecoveryRegistration: cluster %+v has recently experienced a failover (of %+v) and is in active period. It will not be failed over again. You may acknowledge the failure on this cluster (-c ack-cluster-recoveries) or on %+v (-c ack-instance-recoveries) to remove this blockage", analysisEntry.ClusterDetails.ClusterName, recoveries[0].AnalysisEntry.AnalyzedInstanceKey, recoveries[0].AnalysisEntry.AnalyzedInstanceKey)
			log.Errorf(errMsg)
			return nil, fmt.Errorf(errMsg)
		}
	}
	if !failIfFailedInstanceInActiveRecovery {
		// Implicitly acknowledge this instance's possibly existing active recovery, provided they are completed.
		_, _ = AcknowledgeInstanceCompletedRecoveries(&analysisEntry.AnalyzedInstanceKey, "vtorc", fmt.Sprintf("implicit acknowledge due to user invocation of recovery on same instance: %+v", analysisEntry.AnalyzedInstanceKey))
		// The fact we only acknowledge a completed recovery solves the possible case of two DBAs simultaneously
		// trying to recover the same instance at the same time
	}

	topologyRecovery := NewTopologyRecovery(*analysisEntry)

	topologyRecovery, err := writeTopologyRecovery(topologyRecovery)
	if err != nil {
		log.Error(err)
		return nil, err
	}
	return topologyRecovery, nil
}

// ClearActiveRecoveries clears the "in_active_period" flag for old-enough recoveries, thereby allowing for
// further recoveries on cleared instances.
func ClearActiveRecoveries() error {
	_, err := db.ExecVTOrc(`
			update topology_recovery set
				in_active_period = 0,
				end_active_period_unixtime = UNIX_TIMESTAMP()
			where
				in_active_period = 1
				AND start_active_period < NOW() - INTERVAL ? SECOND
			`,
		config.Config.RecoveryPeriodBlockSeconds,
	)
	if err != nil {
		log.Error(err)
	}
	return err
}

// RegisterBlockedRecoveries writes down currently blocked recoveries, and indicates what recovery they are blocked on.
// Recoveries are blocked thru the in_active_period flag, which comes to avoid flapping.
func RegisterBlockedRecoveries(analysisEntry *inst.ReplicationAnalysis, blockingRecoveries []*TopologyRecovery) error {
	for _, recovery := range blockingRecoveries {
		_, err := db.ExecVTOrc(`
			insert
				into blocked_topology_recovery (
					hostname,
					port,
					cluster_name,
					analysis,
					last_blocked_timestamp,
					blocking_recovery_id
				) values (
					?,
					?,
					?,
					?,
					NOW(),
					?
				)
				on duplicate key update
					cluster_name=values(cluster_name),
					analysis=values(analysis),
					last_blocked_timestamp=values(last_blocked_timestamp),
					blocking_recovery_id=values(blocking_recovery_id)
			`, analysisEntry.AnalyzedInstanceKey.Hostname,
			analysisEntry.AnalyzedInstanceKey.Port,
			analysisEntry.ClusterDetails.ClusterName,
			string(analysisEntry.Analysis),
			recovery.ID,
		)
		if err != nil {
			log.Error(err)
		}
	}
	return nil
}

// ExpireBlockedRecoveries clears listing of blocked recoveries that are no longer actually blocked.
func ExpireBlockedRecoveries() error {
	// Older recovery is acknowledged by now, hence blocked recovery should be released.
	// Do NOTE that the data in blocked_topology_recovery is only used for auditing: it is NOT the data
	// based on which we make automated decisions.

	query := `
		select
				blocked_topology_recovery.hostname,
				blocked_topology_recovery.port
			from
				blocked_topology_recovery
				left join topology_recovery on (blocking_recovery_id = topology_recovery.recovery_id and acknowledged = 0)
			where
				acknowledged is null
		`
	expiredKeys := inst.NewInstanceKeyMap()
	err := db.QueryVTOrc(query, sqlutils.Args(), func(m sqlutils.RowMap) error {
		key := inst.InstanceKey{Hostname: m.GetString("hostname"), Port: m.GetInt("port")}
		expiredKeys.AddKey(key)
		return nil
	})

	for _, expiredKey := range expiredKeys.GetInstanceKeys() {
		_, err := db.ExecVTOrc(`
				delete
					from blocked_topology_recovery
				where
						hostname = ?
						and port = ?
				`,
			expiredKey.Hostname, expiredKey.Port,
		)
		if err != nil {
			log.Error(err)
			return err
		}
	}

	if err != nil {
		log.Error(err)
		return err
	}
	// Some oversampling, if a problem has not been noticed for some time (e.g. the server came up alive
	// before action was taken), expire it.
	// Recall that RegisterBlockedRecoveries continuously updates the last_blocked_timestamp column.
	_, err = db.ExecVTOrc(`
			delete
				from blocked_topology_recovery
				where
					last_blocked_timestamp < NOW() - interval ? second
			`, config.Config.RecoveryPollSeconds*2,
	)
	if err != nil {
		log.Error(err)
	}
	return err
}

// acknowledgeRecoveries sets acknowledged* details and clears the in_active_period flags from a set of entries
func acknowledgeRecoveries(owner string, comment string, markEndRecovery bool, whereClause string, args []any) (countAcknowledgedEntries int64, err error) {
	additionalSet := ``
	if markEndRecovery {
		additionalSet = `
				end_recovery=IFNULL(end_recovery, NOW()),
			`
	}
	query := fmt.Sprintf(`
			update topology_recovery set
				in_active_period = 0,
				end_active_period_unixtime = case when end_active_period_unixtime = 0 then UNIX_TIMESTAMP() else end_active_period_unixtime end,
				%s
				acknowledged = 1,
				acknowledged_at = NOW(),
				acknowledged_by = ?,
				acknowledge_comment = ?
			where
				acknowledged = 0
				and
				%s
		`, additionalSet, whereClause)
	args = append(sqlutils.Args(owner, comment), args...)
	sqlResult, err := db.ExecVTOrc(query, args...)
	if err != nil {
		log.Error(err)
		return 0, err
	}
	rows, err := sqlResult.RowsAffected()
	if err != nil {
		log.Error(err)
	}
	return rows, err
}

// AcknowledgeInstanceCompletedRecoveries marks active and COMPLETED recoveries for given instane as acknowledged.
// This also implied clearing their active period, which in turn enables further recoveries on those topologies
func AcknowledgeInstanceCompletedRecoveries(instanceKey *inst.InstanceKey, owner string, comment string) (countAcknowledgedEntries int64, err error) {
	whereClause := `
			hostname = ?
			and port = ?
			and end_recovery is not null
		`
	return acknowledgeRecoveries(owner, comment, false, whereClause, sqlutils.Args(instanceKey.Hostname, instanceKey.Port))
}

// AcknowledgeCrashedRecoveries marks recoveries whose processing nodes has crashed as acknowledged.
func AcknowledgeCrashedRecoveries() (countAcknowledgedEntries int64, err error) {
	whereClause := `
			in_active_period = 1
			and end_recovery is null
			and concat(processing_node_hostname, ':', processcing_node_token) not in (
				select concat(hostname, ':', token) from node_health
			)
		`
	return acknowledgeRecoveries("vtorc", "detected crashed recovery", true, whereClause, sqlutils.Args())
}

// ResolveRecovery is called on completion of a recovery process and updates the recovery status.
// It does not clear the "active period" as this still takes place in order to avoid flapping.
func writeResolveRecovery(topologyRecovery *TopologyRecovery) error {
	var successorKeyToWrite inst.InstanceKey
	if topologyRecovery.IsSuccessful {
		successorKeyToWrite = *topologyRecovery.SuccessorKey
	}
	_, err := db.ExecVTOrc(`
			update topology_recovery set
				is_successful = ?,
				successor_hostname = ?,
				successor_port = ?,
				successor_alias = ?,
				lost_replicas = ?,
				participating_instances = ?,
				all_errors = ?,
				end_recovery = NOW()
			where
				uid = ?
			`, topologyRecovery.IsSuccessful, successorKeyToWrite.Hostname, successorKeyToWrite.Port,
		topologyRecovery.SuccessorAlias, topologyRecovery.LostReplicas.ToCommaDelimitedList(),
		topologyRecovery.ParticipatingInstanceKeys.ToCommaDelimitedList(),
		strings.Join(topologyRecovery.AllErrors, "\n"),
		topologyRecovery.UID,
	)
	if err != nil {
		log.Error(err)
	}
	return err
}

// readRecoveries reads recovery entry/audit entries from topology_recovery
func readRecoveries(whereCondition string, limit string, args []any) ([]*TopologyRecovery, error) {
	res := []*TopologyRecovery{}
	query := fmt.Sprintf(`
		select
      recovery_id,
			uid,
      hostname,
      port,
      (IFNULL(end_active_period_unixtime, 0) = 0) as is_active,
      start_active_period,
      IFNULL(end_active_period_unixtime, 0) as end_active_period_unixtime,
      IFNULL(end_recovery, '') AS end_recovery,
      is_successful,
      processing_node_hostname,
      processcing_node_token,
      ifnull(successor_hostname, '') as successor_hostname,
      ifnull(successor_port, 0) as successor_port,
      ifnull(successor_alias, '') as successor_alias,
      analysis,
      cluster_name,
      count_affected_replicas,
      participating_instances,
      lost_replicas,
      all_errors,
      acknowledged,
      acknowledged_at,
      acknowledged_by,
      acknowledge_comment,
      last_detection_id
		from
			topology_recovery
		%s
		order by
			recovery_id desc
		%s
		`, whereCondition, limit)
	err := db.QueryVTOrc(query, args, func(m sqlutils.RowMap) error {
		topologyRecovery := *NewTopologyRecovery(inst.ReplicationAnalysis{})
		topologyRecovery.ID = m.GetInt64("recovery_id")
		topologyRecovery.UID = m.GetString("uid")

		topologyRecovery.IsActive = m.GetBool("is_active")
		topologyRecovery.RecoveryStartTimestamp = m.GetString("start_active_period")
		topologyRecovery.RecoveryEndTimestamp = m.GetString("end_recovery")
		topologyRecovery.IsSuccessful = m.GetBool("is_successful")
		topologyRecovery.ProcessingNodeHostname = m.GetString("processing_node_hostname")
		topologyRecovery.ProcessingNodeToken = m.GetString("processcing_node_token")

		topologyRecovery.AnalysisEntry.AnalyzedInstanceKey.Hostname = m.GetString("hostname")
		topologyRecovery.AnalysisEntry.AnalyzedInstanceKey.Port = m.GetInt("port")
		topologyRecovery.AnalysisEntry.Analysis = inst.AnalysisCode(m.GetString("analysis"))
		topologyRecovery.AnalysisEntry.ClusterDetails.ClusterName = m.GetString("cluster_name")
		topologyRecovery.AnalysisEntry.CountReplicas = m.GetUint("count_affected_replicas")

		topologyRecovery.SuccessorKey = &inst.InstanceKey{}
		topologyRecovery.SuccessorKey.Hostname = m.GetString("successor_hostname")
		topologyRecovery.SuccessorKey.Port = m.GetInt("successor_port")
		topologyRecovery.SuccessorAlias = m.GetString("successor_alias")

		topologyRecovery.AnalysisEntry.ClusterDetails.ReadRecoveryInfo()

		topologyRecovery.AllErrors = strings.Split(m.GetString("all_errors"), "\n")
		_ = topologyRecovery.LostReplicas.ReadCommaDelimitedList(m.GetString("lost_replicas"))
		_ = topologyRecovery.ParticipatingInstanceKeys.ReadCommaDelimitedList(m.GetString("participating_instances"))

		topologyRecovery.Acknowledged = m.GetBool("acknowledged")
		topologyRecovery.AcknowledgedAt = m.GetString("acknowledged_at")
		topologyRecovery.AcknowledgedBy = m.GetString("acknowledged_by")
		topologyRecovery.AcknowledgedComment = m.GetString("acknowledge_comment")

		topologyRecovery.LastDetectionID = m.GetInt64("last_detection_id")

		res = append(res, &topologyRecovery)
		return nil
	})

	if err != nil {
		log.Error(err)
	}
	return res, err
}

// ReadInActivePeriodClusterRecovery reads recoveries (possibly complete!) that are in active period.
// (may be used to block further recoveries on this cluster)
func ReadInActivePeriodClusterRecovery(clusterName string) ([]*TopologyRecovery, error) {
	whereClause := `
		where
			in_active_period=1
			and cluster_name=?`
	return readRecoveries(whereClause, ``, sqlutils.Args(clusterName))
}

// ReadInActivePeriodSuccessorInstanceRecovery reads completed recoveries for a given instance, where said instance
// was promoted as result, still in active period (may be used to block further recoveries should this instance die)
func ReadInActivePeriodSuccessorInstanceRecovery(instanceKey *inst.InstanceKey) ([]*TopologyRecovery, error) {
	whereClause := `
		where
			in_active_period=1
			and
				successor_hostname=? and successor_port=?`
	return readRecoveries(whereClause, ``, sqlutils.Args(instanceKey.Hostname, instanceKey.Port))
}

// ReadRecentRecoveries reads latest recovery entries from topology_recovery
func ReadRecentRecoveries(clusterName string, unacknowledgedOnly bool, page int) ([]*TopologyRecovery, error) {
	whereConditions := []string{}
	whereClause := ""
	args := sqlutils.Args()
	if unacknowledgedOnly {
		whereConditions = append(whereConditions, `acknowledged=0`)
	}
	if clusterName != "" {
		whereConditions = append(whereConditions, `cluster_name=?`)
		args = append(args, clusterName)
	}
	if len(whereConditions) > 0 {
		whereClause = fmt.Sprintf("where %s", strings.Join(whereConditions, " and "))
	}
	limit := `
		limit ?
		offset ?`
	args = append(args, config.AuditPageSize, page*config.AuditPageSize)
	return readRecoveries(whereClause, limit, args)
}

// writeTopologyRecoveryStep writes down a single step in a recovery process
func writeTopologyRecoveryStep(topologyRecoveryStep *TopologyRecoveryStep) error {
	sqlResult, err := db.ExecVTOrc(`
			insert ignore
				into topology_recovery_steps (
					recovery_step_id, recovery_uid, audit_at, message
				) values (?, ?, now(), ?)
			`, sqlutils.NilIfZero(topologyRecoveryStep.ID), topologyRecoveryStep.RecoveryUID, topologyRecoveryStep.Message,
	)
	if err != nil {
		log.Error(err)
		return err
	}
	topologyRecoveryStep.ID, err = sqlResult.LastInsertId()
	if err != nil {
		log.Error(err)
	}
	return err
}

// ExpireFailureDetectionHistory removes old rows from the topology_failure_detection table
func ExpireFailureDetectionHistory() error {
	return inst.ExpireTableData("topology_failure_detection", "start_active_period")
}

// ExpireTopologyRecoveryHistory removes old rows from the topology_failure_detection table
func ExpireTopologyRecoveryHistory() error {
	return inst.ExpireTableData("topology_recovery", "start_active_period")
}

// ExpireTopologyRecoveryStepsHistory removes old rows from the topology_failure_detection table
func ExpireTopologyRecoveryStepsHistory() error {
	return inst.ExpireTableData("topology_recovery_steps", "audit_at")
}
