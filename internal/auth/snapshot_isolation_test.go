package auth

import (
	"testing"
)

// ═══════════════════════════════════════════════════════════════════
// 快照隔离（深拷贝）的双向测试
//
// 🔴 Codex 第 53 轮指出：**值拷贝不等于隔离**。
//
//	`Account` 的值拷贝只复制 slice header（ptr/len/cap），
//	底层数组仍与 Store 共享；而写方 `Credit.Packages[:0]`+`append`
//	是**原地改写底层数组** ⇒ 读方的"快照"会跟着变。
//	⇒ 必须有双向断言：改快照不影响 Store，改 Store 不影响旧快照。
// ═══════════════════════════════════════════════════════════════════

func pkg(name string, remain int64) PackageSnapshot {
	return PackageSnapshot{Name: name, Remain: remain, ExpireAt: "2026-12-31"}
}

// TestSnapshotIsolatedFromStoreMutation 守：**改 Store 不影响已有快照**。
//
// 这是 `Credit.Packages[:0]`+`append` 那条路径的直接护栏。
func TestSnapshotIsolatedFromStoreMutation(t *testing.T) {
	st := NewStore()
	st.Put(&Account{
		UID: "u1",
		Credit: CreditSnapshot{
			Known:     true,
			Remaining: 1000,
			Packages:  []PackageSnapshot{pkg("A", 600), pkg("B", 400)},
		},
	})

	// 取快照
	snap, ok := st.Snapshot("u1")
	if !ok {
		t.Fatal("账号应存在")
	}
	if len(snap.Credit.Packages) != 2 {
		t.Fatalf("前置条件：快照应有 2 个包，实际 %d", len(snap.Credit.Packages))
	}

	// 改 Store：**模拟生产里的原地改写**（[:0] + append）
	st.Mutate("u1", func(a *Account) bool {
		a.Credit.Remaining = 7
		a.Credit.Packages = a.Credit.Packages[:0]
		a.Credit.Packages = append(a.Credit.Packages, pkg("C", 7))
		return true
	})

	// 🔴 快照必须**不受影响**（这才是隔离）
	if snap.Credit.Remaining != 1000 {
		t.Errorf("快照 Remaining 被 Store 的写改动了: %d，期望 1000", snap.Credit.Remaining)
	}
	if len(snap.Credit.Packages) != 2 {
		t.Errorf("快照 Packages 数量被改动: %d，期望 2（深拷贝没生效）",
			len(snap.Credit.Packages))
	}
	if len(snap.Credit.Packages) > 0 && snap.Credit.Packages[0].Name != "A" {
		t.Errorf("快照 Packages[0] 被改动: %q，期望 A", snap.Credit.Packages[0].Name)
	}
}

// TestStoreIsolatedFromSnapshotMutation 守：**改快照不影响 Store**（反方向）。
//
// 这条防的是"调用方拿到快照后随手改它，结果污染了仓库"。
// 生产里调用方本不应该改快照，但深拷贝让这个错误**无法造成破坏**。
func TestStoreIsolatedFromSnapshotMutation(t *testing.T) {
	st := NewStore()
	st.Put(&Account{
		UID: "u1",
		Credit: CreditSnapshot{
			Known:     true,
			Remaining: 1000,
			Packages:  []PackageSnapshot{pkg("A", 600), pkg("B", 400)},
		},
	})

	snap, _ := st.Snapshot("u1")
	// 故意改快照（模拟调用方的错误用法）
	snap.Credit.Remaining = 99999
	if len(snap.Credit.Packages) > 0 {
		snap.Credit.Packages[0] = pkg("被污染", 1)
	}
	snap.Status = "rate_limited"

	// 🔴 Store 必须**不受影响**
	after, _ := st.Snapshot("u1")
	if after.Credit.Remaining != 1000 {
		t.Errorf("Store 的 Remaining 被快照改动污染: %d", after.Credit.Remaining)
	}
	if len(after.Credit.Packages) > 0 && after.Credit.Packages[0].Name != "A" {
		t.Errorf("Store 的 Packages[0] 被快照改动污染: %q", after.Credit.Packages[0].Name)
	}
	if after.Status == "rate_limited" {
		t.Error("Store 的 Status 被快照改动污染")
	}
}

// TestListSnapshotsIsolated 守：`ListSnapshots` 同样做了深拷贝。
func TestListSnapshotsIsolated(t *testing.T) {
	st := NewStore()
	st.Put(&Account{
		UID: "u1",
		Credit: CreditSnapshot{
			Known:    true,
			Packages: []PackageSnapshot{pkg("A", 600)},
		},
	})

	snaps := st.ListSnapshots()
	if len(snaps) != 1 {
		t.Fatalf("应有 1 个快照，实际 %d", len(snaps))
	}
	// 改 Store
	st.Mutate("u1", func(a *Account) bool {
		a.Credit.Packages = a.Credit.Packages[:0]
		a.Credit.Packages = append(a.Credit.Packages, pkg("X", 1))
		return true
	})
	if len(snaps[0].Credit.Packages) != 1 || snaps[0].Credit.Packages[0].Name != "A" {
		t.Errorf("ListSnapshots 的切片与 Store 共享底层数组（深拷贝没生效）: %+v",
			snaps[0].Credit.Packages)
	}
}

// TestSnapshotNilPackagesStaysNil 守：`Packages` 为 nil 时保持 nil。
//
// ⚠️ 不要用 `make([]T, 0)` 顶替 nil —— 项目在别处明确区分过
//
//	"空"与"没有"（如 Credit.Known）。nil 表示"从未查到过包"。
func TestSnapshotNilPackagesStaysNil(t *testing.T) {
	st := NewStore()
	st.Put(&Account{UID: "u1"}) // Packages 为 nil

	snap, _ := st.Snapshot("u1")
	if snap.Credit.Packages != nil {
		t.Errorf("原本为 nil 的 Packages 被改成了非 nil（len=%d）—— "+
			"nil 与空切片语义不同，不该顶替", len(snap.Credit.Packages))
	}
}
