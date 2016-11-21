/*
 * Copyright 2016 DGraph Labs, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 * 		http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package worker

import (
	"golang.org/x/net/context"

	"github.com/dgraph-io/dgraph/group"
	"github.com/dgraph-io/dgraph/posting"
	"github.com/dgraph-io/dgraph/task"
	"github.com/dgraph-io/dgraph/types"
	"github.com/dgraph-io/dgraph/x"
)

var (
	emptyUIDList task.List
	emptyResult  task.Result
)

// ProcessTaskOverNetwork is used to process the query and get the result from
// the instance which stores posting list corresponding to the predicate in the
// query.
func ProcessTaskOverNetwork(ctx context.Context, q *task.Query) (*task.Result, error) {
	attr := q.Attr
	gid := group.BelongsTo(attr)
	x.Trace(ctx, "attr: %v groupId: %v", attr, gid)

	if groups().ServesGroup(gid) {
		// No need for a network call, as this should be run from within this instance.
		return processTask(q)
	}

	// Send this over the network.
	// TODO: Send the request to multiple servers as described in Jeff Dean's talk.
	addr := groups().AnyServer(gid)
	pl := pools().get(addr)

	conn, err := pl.Get()
	if err != nil {
		return &emptyResult, x.Wrapf(err, "ProcessTaskOverNetwork: while retrieving connection.")
	}
	defer pl.Put(conn)
	x.Trace(ctx, "Sending request to %v", addr)

	c := NewWorkerClient(conn)
	reply, err := c.ServeTask(ctx, q)
	if err != nil {
		x.TraceError(ctx, x.Wrapf(err, "Error while calling Worker.ServeTask"))
		return &emptyResult, err
	}

	x.Trace(ctx, "Reply from server. length: %v Addr: %v Attr: %v",
		len(reply.UidMatrix), addr, attr)
	return reply, nil
}

// processTask processes the query, accumulates and returns the result.
// TODO: Change input and output to protos from []byte.
func processTask(q *task.Query) (*task.Result, error) {
	attr := q.Attr
	x.AssertTruef(len(q.Uids) == 0 || len(q.Tokens) == 0,
		"At least one of Uids and Term should be empty: %d vs %d", len(q.Uids), len(q.Tokens))

	useTerm := len(q.Tokens) > 0
	var n int
	if useTerm {
		n = len(q.Tokens)
	} else {
		n = len(q.Uids)
	}

	var out task.Result
	for i := 0; i < n; i++ {
		var key []byte
		if useTerm {
			key = types.IndexKey(attr, q.Tokens[i])
		} else {
			key = posting.Key(q.Uids[i], attr)
		}
		// Get or create the posting list for an entity, attribute combination.
		pl, decr := posting.GetOrCreate(key)
		defer decr()

		// If a posting list contains a value, we store that or else we store a nil
		// byte so that processing is consistent later.
		vbytes, vtype, err := pl.Value()

		newValue := &task.Value{ValType: uint32(vtype)}
		if err == nil {
			newValue.Val = vbytes
		} else {
			newValue.Val = x.Nilbyte
		}
		out.Values = append(out.Values, newValue)

		if q.DoCount {
			out.Counts = append(out.Counts, uint32(pl.Length(0)))
			// Add an empty UID list to make later processing consistent
			out.UidMatrix = append(out.UidMatrix, &emptyUIDList)
			continue
		}

		// The more usual case: Getting the UIDs.
		opts := posting.ListOptions{
			AfterUID: uint64(q.AfterUid),
		}
		if q.ToIntersect != nil {
			opts.Intersect = &task.List{Uids: q.ToIntersect}
		}
		out.UidMatrix = append(out.UidMatrix, pl.Uids(opts))
	}
	return &out, nil
}

// ServeTask is used to respond to a query.
func (w *grpcWorker) ServeTask(ctx context.Context, q *task.Query) (*task.Result, error) {
	if ctx.Err() != nil {
		return &emptyResult, ctx.Err()
	}

	gid := group.BelongsTo(q.Attr)
	x.Trace(ctx, "Attribute: %q NumUids: %v groupId: %v ServeTask", q.Attr, len(q.Uids), gid)

	var reply *task.Result
	x.AssertTruef(groups().ServesGroup(gid),
		"attr: %q groupId: %v Request sent to wrong server.", q.Attr, gid)

	c := make(chan error, 1)
	go func() {
		var err error
		reply, err = processTask(q)
		c <- err
	}()

	select {
	case <-ctx.Done():
		return reply, ctx.Err()
	case err := <-c:
		return reply, err
	}
}
