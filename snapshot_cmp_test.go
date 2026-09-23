package matching

import (
	"reflect"
	"testing"
)

// assertSnapshotsEqual 忽略墙钟时间（CreatedAt/Time/Seq 时间戳载荷）后
// 逐字段比较两份引擎快照。
func assertSnapshotsEqual(t *testing.T, want, got EngineSnapshot) {
	t.Helper()
	if want.Seq != got.Seq {
		t.Fatalf("seq mismatch: want=%d got=%d", want.Seq, got.Seq)
	}
	if !reflect.DeepEqual(want.Accounts, got.Accounts) {
		t.Fatalf("accounts mismatch:\nwant=%v\ngot =%v", want.Accounts, got.Accounts)
	}
	if !reflect.DeepEqual(want.Books, got.Books) {
		t.Fatalf("books mismatch:\nwant=%v\ngot =%v", want.Books, got.Books)
	}
	// 订单：比较除 CreatedAt 外的全部字段。
	if len(want.Orders) != len(got.Orders) {
		t.Fatalf("order count want=%d got=%d", len(want.Orders), len(got.Orders))
	}
	for id, wo := range want.Orders {
		go2, ok := got.Orders[id]
		if !ok {
			t.Fatalf("order %s missing in concurrent result", id)
		}
		wo.CreatedAt = 0
		go2.CreatedAt = 0
		if !reflect.DeepEqual(wo, go2) {
			t.Fatalf("order %s mismatch:\nwant=%+v\ngot =%+v", id, wo, go2)
		}
	}
	t.Logf("snapshots equal: seq=%d accounts=%d orders=%d",
		want.Seq, len(want.Accounts), len(want.Orders))
}
