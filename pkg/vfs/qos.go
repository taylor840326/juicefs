/*
 * JuiceFS, Copyright 2024 Juicedata, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package vfs

import (
	"syscall"

	"github.com/juicedata/juicefs/pkg/qos"
)

// waitMeta blocks until one metadata operation is allowed by the client QoS
// limits. It returns EINTR when the operation was canceled while waiting, so
// that an interrupted call does not keep a stalled request alive.
func (v *VFS) waitMeta(ctx Context) syscall.Errno {
	if c := v.qos.Load(); c != nil {
		return c.WaitMeta(ctx)
	}
	return 0
}

// waitData blocks until one data operation of n bytes is allowed by the
// client QoS limits. read selects the direction the bytes are charged to.
func (v *VFS) waitData(ctx Context, read bool, n int64) syscall.Errno {
	if n <= 0 {
		return 0
	}
	if c := v.qos.Load(); c != nil {
		return c.WaitData(ctx, read, n)
	}
	return 0
}

// waitOp blocks until one data operation is allowed, without charging any
// bandwidth. It is used by operations that move data on the server side.
func (v *VFS) waitOp(ctx Context) syscall.Errno {
	if c := v.qos.Load(); c != nil {
		return c.WaitOp(ctx)
	}
	return 0
}

// UpdateQoS applies a partial QoS update at runtime and returns the effective
// configuration. The token buckets are rebuilt and swapped atomically, so new
// operations observe the new limits immediately while operations already
// waiting keep draining the buckets they were charged against. Passing an empty
// update returns the current limits without changing them.
func (v *VFS) UpdateQoS(u qos.Update) qos.Config {
	v.qosMu.Lock()
	defer v.qosMu.Unlock()
	next := v.GetQoS().Apply(u)
	if u.Empty() {
		return next // a query, don't rebuild the buckets
	}
	v.qos.Store(qos.New(&next))
	logger.Infof("QoS limits updated: bandwidth=%d read-bandwidth=%d write-bandwidth=%d iops=%d meta-iops=%d",
		next.Bandwidth, next.ReadBandwidth, next.WriteBandwidth, next.IOPS, next.MetaIOPS)
	return next
}

// GetQoS returns the effective QoS limits of this client.
func (v *VFS) GetQoS() qos.Config {
	return v.qos.Load().Conf()
}

// Ensure the nil controller is safe for GetQoS: (*qos.Controller)(nil).Conf()
// handles nil receivers.
