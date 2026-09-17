package main

import (
	"bytes"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// savePool 落盘后：文件是完整 JSON，且不留下临时文件。
//
// 背景：旧实现直接 os.WriteFile（O_TRUNC）写目标文件，两个并发调用会互相
// 截断，落盘结果是两段 JSON 拼接；loadPool 解析失败会静默换成空池，
// 于是「账号全没了」。这里从外部观察最终产物是否始终可解析。
func TestSavePoolWritesValidJSONAndCleansTemp(t *testing.T) {
	useTemporaryPool(t)

	if _, err := addCustomModel("openai/gpt-4.1-nano"); err != nil {
		t.Fatalf("addCustomModel: %v", err)
	}

	data, err := os.ReadFile(poolPath)
	if err != nil {
		t.Fatalf("read pool file: %v", err)
	}
	var p AccountPool
	if err := json.Unmarshal(data, &p); err != nil {
		t.Fatalf("落盘内容不是完整 JSON: %v (content=%q)", err, data)
	}
	if len(p.CustomModels) != 1 || p.CustomModels[0] != "openai/gpt-4.1-nano" {
		t.Fatalf("落盘的 customModels = %v", p.CustomModels)
	}

	entries, err := os.ReadDir(filepath.Dir(poolPath))
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Fatalf("残留临时文件：%s", e.Name())
		}
	}
}

// 回归测试：savePool（池锁外调用）与 savePoolLocked（池锁内调用）并发时不得死锁。
//
// savePool 的设计是「池锁内 marshal、池锁外落盘」——两个锁从不嵌套。
// 如果哪天改回「先取 saveMu 再取 poolMu」，就会和持有 poolMu 后再取 saveMu 的
// savePoolLocked 形成环等锁。死锁表现为测试卡住而不是失败，所以用超时兜底。
func TestSavePoolConcurrentWithPoolMuHeld(t *testing.T) {
	useTemporaryPool(t)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 5; i++ {
			poolMu.Lock()
			err := savePoolLocked()
			poolMu.Unlock()
			if err != nil {
				t.Errorf("savePoolLocked: %v", err)
				return
			}
		}
	}()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 5; i++ {
			if err := savePool(); err != nil {
				t.Errorf("savePool: %v", err)
				break
			}
		}
		wg.Wait()
	}()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("savePool 与 savePoolLocked 并发时死锁")
	}
}

// 带 UTF-8 BOM 的账号池文件必须能正常加载。
//
// 背景：Windows 记事本、PowerShell 的 Set-Content -Encoding UTF8 都会写 BOM，
// 而 encoding/json 遇到 BOM 直接报错；旧实现解析失败会静默换成空池，
// 随后任何一次写操作都会把用户的账号文件覆盖成空池。
func TestLoadPoolAcceptsUTF8BOM(t *testing.T) {
	useTemporaryPool(t)

	raw := append([]byte{0xEF, 0xBB, 0xBF},
		[]byte(`{"accounts":[{"accountId":"acc_bom","email":"bom@test.com","refreshToken":"rt","status":"active"}],"currentIdx":0,"cooldownMinutes":7}`)...)
	if err := os.WriteFile(poolPath, raw, 0600); err != nil {
		t.Fatalf("write pool file: %v", err)
	}

	p := loadPool()
	if len(p.Accounts) != 1 || p.Accounts[0].AccountID != "acc_bom" {
		t.Fatalf("带 BOM 的账号池未被加载：accounts=%v", p.Accounts)
	}
	if got := cooldownMinutes(); got != 7 {
		t.Fatalf("cooldownMinutes = %d, want 7", got)
	}
}

// 无法解析的账号池文件必须原样保留，不能被后续写操作覆盖成空池。
func TestLoadPoolKeepsUnparsableFile(t *testing.T) {
	useTemporaryPool(t)

	broken := []byte(`{"accounts":[ THIS IS NOT JSON`)
	if err := os.WriteFile(poolPath, broken, 0600); err != nil {
		t.Fatalf("write pool file: %v", err)
	}

	if p := loadPool(); len(p.Accounts) != 0 {
		t.Fatalf("损坏文件应降级为空池，实际 %d 个账号", len(p.Accounts))
	}
	if _, err := addCustomModel("openai/gpt-4.1-nano"); err != nil {
		t.Fatalf("addCustomModel: %v", err)
	}

	matches, err := filepath.Glob(poolPath + ".corrupt-*")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("应保留 1 个损坏文件备份，实际 %d 个", len(matches))
	}
	got, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if string(got) != string(broken) {
		t.Fatalf("备份内容被改动：%q", got)
	}
}

// 空账号池文件不是损坏：不该留下 .corrupt-* 备份文件。
func TestLoadPoolTreatsEmptyFileAsEmptyPool(t *testing.T) {
	useTemporaryPool(t)

	if err := os.WriteFile(poolPath, []byte("   \n"), 0600); err != nil {
		t.Fatalf("write pool file: %v", err)
	}
	if p := loadPool(); len(p.Accounts) != 0 {
		t.Fatalf("空文件应为空池，实际 %d 个账号", len(p.Accounts))
	}
	matches, err := filepath.Glob(poolPath + ".corrupt-*")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("空文件不应产生备份，实际 %v", matches)
	}
}

// rename 不被允许时必须退回原地写，而不是让每一次保存都失败。
//
// 真实触发场景：docker-compose 把账号池按单文件 bind mount 挂进容器
// （./.cline-accounts.json:/app/.cline-accounts.json），此时 rename 覆盖挂载点
// 返回 EBUSY「device or resource busy」。旧实现下禁用/启用账号、改配置、
// 甚至每次代理请求的用量落盘都会报错。
func TestWritePoolFileFallsBackWhenRenameUnsupported(t *testing.T) {
	useTemporaryPool(t)

	original := renameFile
	renameFile = func(_, _ string) error { return syscall.EBUSY }
	t.Cleanup(func() {
		renameFile = original
		atomicReplaceUnsupportedFor = ""
	})

	if err := savePool(); err != nil {
		t.Fatalf("rename 不可用时应退回原地写，实际报错: %v", err)
	}
	if atomicReplaceUnsupportedFor != poolPath {
		t.Fatal("应记住该路径不支持原子替换，避免每次落盘都白写临时文件")
	}

	// 内容确实写进去了
	data, err := os.ReadFile(poolPath)
	if err != nil {
		t.Fatalf("read pool file: %v", err)
	}
	var p AccountPool
	if err := json.Unmarshal(data, &p); err != nil {
		t.Fatalf("原地写的内容不是完整 JSON: %v (content=%q)", err, data)
	}

	// 退回路径上第二次保存也必须成功
	if err := setCooldownMinutes(7); err != nil {
		t.Fatalf("第二次保存失败: %v", err)
	}
	if got := cooldownMinutes(); got != 7 {
		t.Fatalf("cooldownMinutes = %d, want 7", got)
	}

	// 不能留下临时文件
	matches, err := filepath.Glob(poolPath + ".*.tmp")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("rename 失败后应清掉临时文件，实际残留 %v", matches)
	}
}

// 退回原地写必须真的截断：新内容比旧内容短时不能留下旧字节尾巴。
//
// 否则文件变成「新 JSON + 旧尾巴」，解析失败——下次启动 loadPool 会把它
// 当成损坏文件备份走，用户看到的是「账号全没了」。
func TestInPlaceWriteTruncatesStaleTail(t *testing.T) {
	useTemporaryPool(t)

	original := renameFile
	renameFile = func(_, _ string) error { return syscall.EBUSY }
	t.Cleanup(func() {
		renameFile = original
		atomicReplaceUnsupportedFor = ""
	})

	for _, id := range []string{"a/one", "a/two", "a/three", "a/four", "a/five"} {
		if _, err := addCustomModel(id); err != nil {
			t.Fatalf("addCustomModel(%s): %v", id, err)
		}
	}
	long, err := os.ReadFile(poolPath)
	if err != nil {
		t.Fatalf("read pool file: %v", err)
	}

	// 注意：loadPool() 自己会取 poolMu，必须先取到指针再上锁。
	pl := loadPool()
	poolMu.Lock()
	pl.CustomModels = []string{"a/one"}
	poolMu.Unlock()
	if err := savePool(); err != nil {
		t.Fatalf("savePool: %v", err)
	}

	short, err := os.ReadFile(poolPath)
	if err != nil {
		t.Fatalf("read pool file: %v", err)
	}
	if len(short) >= len(long) {
		t.Fatalf("测试前提不成立：新内容 %d 字节，旧内容 %d 字节", len(short), len(long))
	}
	var p AccountPool
	if err := json.Unmarshal(short, &p); err != nil {
		t.Fatalf("短内容未截断干净（残留旧字节）：%v (content=%q)", err, short)
	}
	if len(p.CustomModels) != 1 || p.CustomModels[0] != "a/one" {
		t.Fatalf("落盘的 customModels = %v", p.CustomModels)
	}
}

// 回退提示只能出现一次：它会在每次落盘时刷一行的话，代理运行期间日志会被淹没
// （每次请求的用量落盘都会走 savePool）。
//
// 这条测试同时锁住两件事：
//  1. 提示只出现一次（atomicReplaceUnsupportedFor 生效）；
//  2. 第二次之后的落盘仍然成功（没有因为跳过 rename 而写不进去）。
func TestBindMountFallbackLogsOnlyOnce(t *testing.T) {
	useTemporaryPool(t)

	originalRename := renameFile
	renameFile = func(_, _ string) error { return syscall.EBUSY }

	// 全局 logger 与全局 renameFile 都要还原，否则会污染其它测试。
	originalOut := log.Writer()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() {
		log.SetOutput(originalOut)
		renameFile = originalRename
		atomicReplaceUnsupportedFor = ""
	})

	// 模拟「运行中的多次落盘」。
	const saves = 4
	for i := 0; i < saves; i++ {
		if err := savePool(); err != nil {
			t.Fatalf("第 %d 次落盘失败: %v", i+1, err)
		}
	}

	got := strings.Count(buf.String(), "using in-place writes")
	if got != 1 {
		t.Errorf("回退提示出现 %d 次，应恰好 1 次；日志内容：%q", got, buf.String())
	}
	// 措辞必须让人知道无需处理，否则会被当成故障。
	if !strings.Contains(buf.String(), "no action needed") {
		t.Errorf("回退提示应说明无需处理，实际：%q", buf.String())
	}
}
