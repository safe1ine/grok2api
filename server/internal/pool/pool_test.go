package pool

import (
	"sync"
	"testing"
	"time"
)

func TestAcquireReleaseExclusive(t *testing.T) {
	p := New(nil, nil)
	p.AddAccount(1, "a@x.com", "rt1")
	p.AddAccount(2, "b@x.com", "rt2")

	a1, err := p.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	a2, err := p.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	if a1.ID == a2.ID {
		t.Fatal("不同请求拿到同一账号")
	}
	if _, err := p.Acquire(); err == nil {
		t.Fatal("全部借出后应无可用账号")
	}
	p.Release(a1, time.Now())
	a3, err := p.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	if a3.ID != a1.ID {
		t.Fatalf("应归还 a1，实际得到 %d", a3.ID)
	}
}

func TestRebuildHeapNoDuplicate(t *testing.T) {
	p := New(nil, nil)
	p.AddAccount(1, "a", "rt")
	p.AddAccount(2, "b", "rt")

	a1, _ := p.Acquire() // checkout 账号1，堆里只剩账号2
	p.Remove(2)          // 删除账号2，触发 rebuildHeap，不应把已 checkout 的账号1重复入堆
	p.Release(a1, time.Now())

	a, err := p.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != 1 {
		t.Fatalf("期望账号1，得到 %d", a.ID)
	}
	if _, err := p.Acquire(); err == nil {
		t.Fatal("池中不应有多余账号（重复入堆 bug）")
	}
}

func TestConcurrentAcquireDistinct(t *testing.T) {
	p := New(nil, nil)
	const N = 200
	for i := 0; i < N; i++ {
		p.AddAccount(int64(i+1), "a", "rt")
	}

	var mu sync.Mutex
	got := map[int64]int{}
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a, err := p.Acquire()
			if err != nil {
				return
			}
			mu.Lock()
			got[a.ID]++
			mu.Unlock()
			time.Sleep(time.Millisecond)
			p.Release(a, time.Now())
		}()
	}
	wg.Wait()

	for id, c := range got {
		if c != 1 {
			t.Fatalf("账号 %d 被并发使用 %d 次", id, c)
		}
	}
}
