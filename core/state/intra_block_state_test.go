// Copyright 2016 The go-ethereum Authors
// (original work)
// Copyright 2024 The Erigon Authors
// (modifications)
// This file is part of Erigon.
//
// Erigon is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// Erigon is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with Erigon. If not, see <http://www.gnu.org/licenses/>.

package state

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"math/big"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"testing/quick"

	"github.com/holiman/uint256"

	"github.com/erigontech/erigon-lib/common"
	"github.com/erigontech/erigon-lib/common/datadir"
	"github.com/erigontech/erigon-lib/kv/memdb"
	"github.com/erigontech/erigon-lib/kv/rawdbv3"
	"github.com/erigontech/erigon-lib/kv/temporal"
	"github.com/erigontech/erigon-lib/log/v3"
	stateLib "github.com/erigontech/erigon-lib/state"
	"github.com/erigontech/erigon-lib/types"
	"github.com/erigontech/erigon/core/tracing"
)

func TestSnapshotRandom(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}

	t.Parallel()
	config := &quick.Config{MaxCount: 10}
	err := quick.Check((*snapshotTest).run, config)
	if cerr, ok := err.(*quick.CheckError); ok {
		test := cerr.In[0].(*snapshotTest)
		t.Errorf("%v:\n%s", test.err, test)
	} else if err != nil {
		t.Error(err)
	}
}

// A snapshotTest checks that reverting IntraBlockState snapshots properly undoes all changes
// captured by the snapshot. Instances of this test with pseudorandom content are created
// by Generate.
//
// The test works as follows:
//
// A new state is created and all actions are applied to it. Several snapshots are taken
// in between actions. The test then reverts each snapshot. For each snapshot the actions
// leading up to it are replayed on a fresh, empty state. The behaviour of all public
// accessor methods on the reverted state must match the return value of the equivalent
// methods on the replayed state.
type snapshotTest struct {
	addrs     []common.Address // all account addresses
	actions   []testAction     // modifications to the state
	snapshots []int            // actions indexes at which snapshot is taken
	err       error            // failure details are reported through this field
}

type testAction struct {
	name   string
	fn     func(testAction, *IntraBlockState)
	args   []int64
	noAddr bool
}

// newTestAction creates a random action that changes state.
func newTestAction(addr common.Address, r *rand.Rand) testAction {
	actions := []testAction{
		{
			name: "SetBalance",
			fn: func(a testAction, s *IntraBlockState) {
				s.SetBalance(addr, *uint256.NewInt(uint64(a.args[0])), tracing.BalanceChangeUnspecified)
			},
			args: make([]int64, 1),
		},
		{
			name: "AddBalance",
			fn: func(a testAction, s *IntraBlockState) {
				s.AddBalance(addr, uint256.NewInt(uint64(a.args[0])), tracing.BalanceChangeUnspecified)
			},
			args: make([]int64, 1),
		},
		{
			name: "SetNonce",
			fn: func(a testAction, s *IntraBlockState) {
				s.SetNonce(addr, uint64(a.args[0]))
			},
			args: make([]int64, 1),
		},
		{
			name: "SetState",
			fn: func(a testAction, s *IntraBlockState) {
				var key common.Hash
				binary.BigEndian.PutUint16(key[:], uint16(a.args[0]))
				val := uint256.NewInt(uint64(a.args[1]))
				s.SetState(addr, key, *val)
			},
			args: make([]int64, 2),
		},
		{
			name: "SetCode",
			fn: func(a testAction, s *IntraBlockState) {
				code := make([]byte, 16)
				binary.BigEndian.PutUint64(code, uint64(a.args[0]))
				binary.BigEndian.PutUint64(code[8:], uint64(a.args[1]))
				s.SetCode(addr, code)
			},
			args: make([]int64, 2),
		},
		{
			name: "CreateAccount",
			fn: func(a testAction, s *IntraBlockState) {
				s.CreateAccount(addr, true)
			},
		},
		{
			name: "Selfdestruct",
			fn: func(a testAction, s *IntraBlockState) {
				s.Selfdestruct(addr)
			},
		},
		{
			name: "AddRefund",
			fn: func(a testAction, s *IntraBlockState) {
				s.AddRefund(uint64(a.args[0]))
			},
			args:   make([]int64, 1),
			noAddr: true,
		},
		{
			name: "AddLog",
			fn: func(a testAction, s *IntraBlockState) {
				data := make([]byte, 2)
				binary.BigEndian.PutUint16(data, uint16(a.args[0]))
				s.AddLog(&types.Log{Address: addr, Data: data})
			},
			args: make([]int64, 1),
		},
		{
			name: "AddAddressToAccessList",
			fn: func(a testAction, s *IntraBlockState) {
				s.AddAddressToAccessList(addr)
			},
		},
		{
			name: "AddSlotToAccessList",
			fn: func(a testAction, s *IntraBlockState) {
				s.AddSlotToAccessList(addr,
					common.Hash{byte(a.args[0])})
			},
			args: make([]int64, 1),
		},
		{
			name: "SetTransientState",
			fn: func(a testAction, s *IntraBlockState) {
				var key common.Hash
				binary.BigEndian.PutUint16(key[:], uint16(a.args[0]))
				val := uint256.NewInt(uint64(a.args[1]))
				s.SetTransientState(addr, key, *val)
			},
			args: make([]int64, 2),
		},
	}
	action := actions[r.Intn(len(actions))]
	var nameargs []string //nolint:prealloc
	if !action.noAddr {
		nameargs = append(nameargs, addr.Hex())
	}
	for i := range action.args {
		action.args[i] = rand.Int63n(100)
		nameargs = append(nameargs, fmt.Sprint(action.args[i]))
	}
	action.name += strings.Join(nameargs, ", ")
	return action
}

// Generate returns a new snapshot test of the given size. All randomness is
// derived from r.
func (*snapshotTest) Generate(r *rand.Rand, size int) reflect.Value {
	// Generate random actions.
	addrs := make([]common.Address, 50)
	for i := range addrs {
		addrs[i][0] = byte(i)
	}
	actions := make([]testAction, size)
	for i := range actions {
		addr := addrs[r.Intn(len(addrs))]
		actions[i] = newTestAction(addr, r)
	}
	// Generate snapshot indexes.
	nsnapshots := int(math.Sqrt(float64(size)))
	if size > 0 && nsnapshots == 0 {
		nsnapshots = 1
	}
	snapshots := make([]int, nsnapshots)
	snaplen := len(actions) / nsnapshots
	for i := range snapshots {
		// Try to place the snapshots some number of actions apart from each other.
		snapshots[i] = (i * snaplen) + r.Intn(snaplen)
	}
	return reflect.ValueOf(&snapshotTest{addrs, actions, snapshots, nil})
}

func (test *snapshotTest) String() string {
	out := new(bytes.Buffer)
	sindex := 0
	for i, action := range test.actions {
		if len(test.snapshots) > sindex && i == test.snapshots[sindex] {
			fmt.Fprintf(out, "---- snapshot %d ----\n", sindex)
			sindex++
		}
		fmt.Fprintf(out, "%4d: %s\n", i, action.name)
	}
	return out.String()
}

func (test *snapshotTest) run() bool {
	// Run all actions and create snapshots.
	db := memdb.NewStateDB("")
	defer db.Close()

	agg, err := stateLib.NewAggregator(context.Background(), datadir.New(""), 16, db, log.New())
	if err != nil {
		test.err = err
		return false
	}
	defer agg.Close()

	_db, err := temporal.New(db, agg)
	if err != nil {
		test.err = err
		return false
	}

	tx, err := _db.BeginTemporalRw(context.Background()) //nolint:gocritic
	if err != nil {
		test.err = err
		return false
	}
	defer tx.Rollback()

	err = rawdbv3.TxNums.Append(tx, 1, 1)
	if err != nil {
		test.err = err
		return false
	}
	var (
		state        = New(NewReaderV3(tx))
		snapshotRevs = make([]int, len(test.snapshots))
		sindex       = 0
	)
	for i, action := range test.actions {
		if len(test.snapshots) > sindex && i == test.snapshots[sindex] {
			snapshotRevs[sindex] = state.Snapshot()
			sindex++
		}
		action.fn(action, state)
	}
	// Revert all snapshots in reverse order. Each revert must yield a state
	// that is equivalent to fresh state with all actions up the snapshot applied.
	for sindex--; sindex >= 0; sindex-- {
		checkstate := New(NewReaderV3(tx))
		for _, action := range test.actions[:test.snapshots[sindex]] {
			action.fn(action, checkstate)
		}
		state.RevertToSnapshot(snapshotRevs[sindex])
		if err := test.checkEqual(state, checkstate); err != nil {
			test.err = fmt.Errorf("state mismatch after revert to snapshot %d\n%w", sindex, err)
			return false
		}
	}
	return true
}

// checkEqual checks that methods of state and checkstate return the same values.
func (test *snapshotTest) checkEqual(state, checkstate *IntraBlockState) error {
	for _, addr := range test.addrs {
		addr := addr // pin
		var err error
		checkeq := func(op string, a, b interface{}) bool {
			if err == nil && !reflect.DeepEqual(a, b) {
				err = fmt.Errorf("got %s(%s) == %v, want %v", op, addr.Hex(), a, b)
				return false
			}
			return true
		}
		checkeqBigInt := func(op string, a, b *big.Int) bool {
			if err == nil && a.Cmp(b) != 0 {
				err = fmt.Errorf("got %s(%s) == %d, want %d", op, addr.Hex(), a, b)
				return false
			}
			return true
		}
		// Check basic accessor methods.
		se, err := state.Exist(addr)
		if err != nil {
			return err
		}
		ce, err := checkstate.Exist(addr)
		if err != nil {
			return err
		}
		if !checkeq("Exist", se, ce) {
			return err
		}
		ssd, err := state.HasSelfdestructed(addr)
		if err != nil {
			return err
		}
		csd, err := checkstate.HasSelfdestructed(addr)
		if err != nil {
			return err
		}
		checkeq("HasSelfdestructed", ssd, csd)
		sb, err := state.GetBalance(addr)
		if err != nil {
			return err
		}
		cb, err := checkstate.GetBalance(addr)
		if err != nil {
			return err
		}
		checkeqBigInt("GetBalance", sb.ToBig(), cb.ToBig())
		sn, err := state.GetNonce(addr)
		if err != nil {
			return err
		}
		cn, err := checkstate.GetNonce(addr)
		if err != nil {
			return err
		}
		checkeq("GetNonce", sn, cn)
		sc, err := state.GetCode(addr)
		if err != nil {
			return err
		}
		cc, err := checkstate.GetCode(addr)
		if err != nil {
			return err
		}
		checkeq("GetCode", sc, cc)
		sch, err := state.GetCodeHash(addr)
		if err != nil {
			return err
		}
		cch, err := checkstate.GetCodeHash(addr)
		if err != nil {
			return err
		}
		checkeq("GetCodeHash", sch, cch)
		scs, err := state.GetCodeSize(addr)
		if err != nil {
			return err
		}
		ccs, err := checkstate.GetCodeSize(addr)
		if err != nil {
			return err
		}
		checkeq("GetCodeSize", scs, ccs)
		// Check storage.
		obj, err := state.getStateObject(addr)
		if err != nil {
			return err
		}
		if obj != nil {
			for key, value := range obj.dirtyStorage {
				var out uint256.Int
				checkstate.GetState(addr, key, &out)
				if !checkeq("GetState("+key.Hex()+")", out, value) {
					return err
				}
			}
		}
		obj, err = checkstate.getStateObject(addr)
		if err != nil {
			return err
		}
		if obj != nil {
			for key, value := range obj.dirtyStorage {
				var out uint256.Int
				state.GetState(addr, key, &out)
				if !checkeq("GetState("+key.Hex()+")", out, value) {
					return err
				}
			}
		}
	}

	if state.GetRefund() != checkstate.GetRefund() {
		return fmt.Errorf("got GetRefund() == %d, want GetRefund() == %d",
			state.GetRefund(), checkstate.GetRefund())
	}
	if !reflect.DeepEqual(state.GetRawLogs(0), checkstate.GetRawLogs(0)) {
		return fmt.Errorf("got GetRawLogs(common.Hash{}) == %v, want GetRawLogs(common.Hash{}) == %v",
			state.GetRawLogs(0), checkstate.GetRawLogs(0))
	}
	return nil
}

func TestTransientStorage(t *testing.T) {
	t.Parallel()
	state := New(nil)

	key := common.Hash{0x01}
	value := uint256.NewInt(2)
	addr := common.Address{}

	state.SetTransientState(addr, key, *value)
	if exp, got := 1, state.journal.length(); exp != got {
		t.Fatalf("journal length mismatch: have %d, want %d", got, exp)
	}
	// the retrieved value should equal what was set
	if got := state.GetTransientState(addr, key); got != *value {
		t.Fatalf("transient storage mismatch: have %x, want %x", got, value)
	}

	// revert the transient state being set and then check that the
	// value is now the empty hash
	state.journal.revert(state, 0)
	if got, exp := state.GetTransientState(addr, key), (*uint256.NewInt(0)); exp != got {
		t.Fatalf("transient storage mismatch: have %x, want %x", got, exp)
	}
}
