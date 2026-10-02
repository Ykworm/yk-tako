// 本文件：Store（看板正本）的单元测试——CRUD、deleted 过滤、并发安全。
package taskboard

import (
	"fmt"
	"sort"
	"strconv"
	"sync"
	"testing"
)

// TestStoreCreateAssignsSequentialIDs 验证自增 id 从 "1" 开始、字段落入、返回的是副本。
func TestStoreCreateAssignsSequentialIDs(t *testing.T) {
	s := NewStore()
	first := s.Create("查时间", "调用时钟工具", "查时间")
	second := s.Create("算乘法", "", "")
	if first.ID != "1" || second.ID != "2" {
		t.Fatalf("id 应自增: %q %q", first.ID, second.ID)
	}
	if first.Status != StatusPending || first.Description != "调用时钟工具" || first.ActiveForm != "查时间" {
		t.Fatalf("字段落入不符: %+v", first)
	}
	// 返回副本：改返回值不影响正本
	first.Subject = "改过的"
	got, err := s.Get("1")
	if err != nil || got.Subject != "查时间" {
		t.Fatalf("Create 应返回副本: %+v err=%v", got, err)
	}
}

// TestStoreUpdatePatch 验证按 id patch：空串字段不动原值，status 可改。
func TestStoreUpdatePatch(t *testing.T) {
	s := NewStore()
	s.Create("查时间", "描述", "进行中")
	got, err := s.Update("1", Patch{Status: "in_progress"})
	if err != nil || got.Status != StatusInProgress {
		t.Fatalf("status 应改为 in_progress: %+v err=%v", got, err)
	}
	got, err = s.Update("1", Patch{Subject: "看时间"})
	if err != nil || got.Subject != "看时间" || got.Description != "描述" || got.Status != StatusInProgress {
		t.Fatalf("空串字段不应动原值: %+v err=%v", got, err)
	}
}

// TestStoreUpdateRejectsBadInput 验证非法 id 与非法 status 都报错。
func TestStoreUpdateRejectsBadInput(t *testing.T) {
	s := NewStore()
	s.Create("查时间", "", "")
	if _, err := s.Update("99", Patch{Status: "completed"}); err == nil {
		t.Fatal("不存在的 id 应报错")
	}
	if _, err := s.Update("1", Patch{Status: "doing"}); err == nil {
		t.Fatal("非法 status 应报错")
	}
}

// TestStoreListFiltersDeleted 验证 List 不含 deleted、按创建序；Snapshot 含 deleted。
func TestStoreListFiltersDeleted(t *testing.T) {
	s := NewStore()
	s.Create("一", "", "")
	s.Create("二", "", "")
	s.Create("三", "", "")
	if _, err := s.Update("2", Patch{Status: "deleted"}); err != nil {
		t.Fatal(err)
	}
	list := s.List()
	if len(list) != 2 || list[0].ID != "1" || list[1].ID != "3" {
		t.Fatalf("List 应过滤 deleted 且按创建序: %+v", list)
	}
	if snap := s.Snapshot(); len(snap) != 3 {
		t.Fatalf("Snapshot 应含 deleted: %+v", snap)
	}
}

// TestStoreGetUnknownID 验证 Get 不存在的 id 报错。
func TestStoreGetUnknownID(t *testing.T) {
	if _, err := NewStore().Get("1"); err == nil {
		t.Fatal("不存在的 id 应报错")
	}
}

// TestStoreConcurrentCreate 验证并发 Create 写不坏：100 个 goroutine 下来 id 恰好是 1..100。
// 用 go test -race 跑最有意义。
func TestStoreConcurrentCreate(t *testing.T) {
	s := NewStore()
	var wg sync.WaitGroup
	ids := make(chan string, 100)
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ids <- s.Create(fmt.Sprintf("任务%d", i), "", "").ID
		}(i)
	}
	wg.Wait()
	close(ids)
	seen := map[string]bool{}
	for id := range ids {
		if seen[id] {
			t.Fatalf("id 重复: %s", id)
		}
		seen[id] = true
	}
	if len(seen) != 100 {
		t.Fatalf("应有 100 个唯一 id，实际 %d", len(seen))
	}
	// 按数值排序比上下限：字符串排序会把 "99" 排在 "100" 之后，比不出上限。
	nums := make([]int, 0, len(seen))
	for id := range seen {
		n, err := strconv.Atoi(id)
		if err != nil {
			t.Fatalf("id 不是数字: %q", id)
		}
		nums = append(nums, n)
	}
	sort.Ints(nums)
	if nums[0] != 1 || nums[len(nums)-1] != 100 {
		t.Fatalf("id 应覆盖 1..100: %d ... %d", nums[0], nums[len(nums)-1])
	}
}
