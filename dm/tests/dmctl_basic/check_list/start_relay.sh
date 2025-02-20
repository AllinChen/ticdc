#!/bin/bash

function start_relay_empty_arg() {
	run_dm_ctl $WORK_DIR "127.0.0.1:$MASTER_PORT" \
		"start-relay" \
		"start-relay <-s source-id> <worker-name> \[...worker-name\]" 1
}

function start_relay_wrong_arg() {
	run_dm_ctl $WORK_DIR "127.0.0.1:$MASTER_PORT" \
		"start-relay wrong_arg" \
		"must specify one source (\`-s\` \/ \`--source\`)" 1
}

function start_relay_without_worker() {
	run_dm_ctl $WORK_DIR "127.0.0.1:$MASTER_PORT" \
		"start-relay -s $SOURCE_ID1" \
		"must specify at least one worker" 1
}

function start_relay_success() {
	run_dm_ctl $WORK_DIR "127.0.0.1:$MASTER_PORT" \
		"start-relay -s $SOURCE_ID1 worker1" \
		"\"result\": true" 2 \
		"\"source\": \"$SOURCE_ID1\"" 1 \
		"\"worker\": \"worker1\"" 1

	run_dm_ctl $WORK_DIR "127.0.0.1:$MASTER_PORT" \
		"start-relay -s $SOURCE_ID2 worker2" \
		"\"result\": true" 2 \
		"\"source\": \"$SOURCE_ID2\"" 1 \
		"\"worker\": \"worker2\"" 1
}

function start_relay_fail() {
	run_dm_ctl $WORK_DIR "127.0.0.1:$MASTER_PORT" \
		"start-relay -s $SOURCE_ID1 worker2" \
		"these workers \[worker2\] have bound for another sources \[$SOURCE_ID2\] respectively" 1
}

function start_relay_on_offline_worker() {
	run_dm_ctl $WORK_DIR "127.0.0.1:$MASTER_PORT" \
		"start-relay -s $SOURCE_ID2 worker2" \
		"\"result\": true" 2 \
		"\"msg\": \"source relay is operated but the bounded worker is offline\"" 1 \
		"\"source\": \"$SOURCE_ID2\"" 1 \
		"\"worker\": \"worker2\"" 1
}
