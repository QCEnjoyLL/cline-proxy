package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)
// 解析失败时留副本必须不依赖 rename。
//
// 背景：docker-compose 把账号池按单文件 bind mount 挂进容器时，rename 一个挂载点
// 会被内核拒绝（EBUSY，就是日志里 writePoolFile 那条提示）。原先的备份实现用的正是
// os.Rename，所以在那种部署下「写盘」有兜底、「备份」却没有——一旦文件损坏，
// 就只剩一条「could not be backed up」然后进程继续以空池运行，
// 随后任意一次写操作都会把用户唯一的账号文件覆盖成空池。
func TestBackupUnreadablePoolWorksWithoutRename(t *testing.T) {
	useTemporaryPool(t)

	// 模拟 bind mount：rename 一律不可用
	originalRename := renameFile
	renameFile = func(_, _ string) error { return syscall.EBUSY }
	t.Cleanup(func() {
		renameFile = originalRename
		atomicReplaceUnsupportedFor = ""
		poolSaveBlocked = atomic.Value{}
	})

	broken := []byte(`{"accounts":[ THIS IS NOT JSON`)
	if err := os.WriteFile(poolPath, broken, 0600); err != nil {
		t.Fatalf("write pool file: %v", err)
	}

	p := loadPool()
	if len(p.Accounts) != 0 {
		t.Fatalf("损坏文件应降级为空池，实际 %d 个账号", len(p.Accounts))
	}
	// 关键断言：备份必须成功（不能因为 rename 不可用而失败）
	if reason, _ := poolSaveBlocked.Load().(string); reason != "" {
		t.Fatalf("rename 不可用时备份仍应成功，但保存被禁用了：%s", reason)
	}

	matches, err := filepath.Glob(poolPath + ".corrupt-*")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("应留下 1 个副本，实际 %d 个", len(matches))
	}
	// 副本内容必须与原始损坏内容逐字节一致
	got, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if string(got) != string(broken) {
		t.Fatalf("副本内容被改动：%q", got)
	}

	// 坏内容必须已从原路径清掉：否则它以「空池」身份继续存在，
	// 下一次保存就会把它覆盖成真正的空池（账号丢失）。
	after, err := os.ReadFile(poolPath)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read pool file: %v", err)
	}
	if len(strings.TrimSpace(string(after))) != 0 {
		t.Fatalf("坏内容应已清空，实际仍有 %d 字节", len(after))
	}

	// 此时保存应当恢复正常，并且落盘的是合法 JSON
	if _, err := addCustomModel("openai/gpt-4.1-nano"); err != nil {
		t.Fatalf("addCustomModel: %v", err)
	}
	data, err := os.ReadFile(poolPath)
	if err != nil {
		t.Fatalf("read pool file: %v", err)
	}
	var pool AccountPool
	if err := json.Unmarshal(data, &pool); err != nil {
		t.Fatalf("恢复后落盘内容不是合法 JSON: %v (%q)", err, data)
	}
	if len(pool.CustomModels) != 1 {
		t.Fatalf("落盘的 customModels = %v", pool.CustomModels)
	}
}

// 连副本都留不下来时必须禁止后续落盘，而不是带着空池继续跑。
//
// 这是最后一道防线：宁可让保存全部失败（文件原样留在磁盘上等人处理），
// 也不能让某次保存把用户唯一的账号文件覆盖掉。
func TestPoolSaveIsBlockedWhenBackupImpossible(t *testing.T) {
	useTemporaryPool(t)
	// 本测试会把全局的「禁止落盘」标志置位，必须清理，否则污染后续所有测试
	// （生产环境里这个标志一旦置位也不会自愈——见 loadPool 的注释）。
	t.Cleanup(func() { poolSaveBlocked = atomic.Value{} })

	// 可确定地让备份失败：把备份目标路径先建成目录。
	// 副本文件名是 "<poolPath>.corrupt-<unix 秒>"，提前把它建成目录后，
	// os.OpenFile 必然失败——比依赖目录权限可靠（本机以普通用户运行，
	// 只读目录仍可写入，测不出这条分支）。
	backupDir := fmt.Sprintf("%s.corrupt-%d", poolPath, time.Now().Unix())
	if err := os.Mkdir(backupDir, 0700); err != nil {
		t.Fatalf("mkdir 占位目录: %v", err)
	}
	broken := []byte(`{"accounts":[ broken`)
	if err := os.WriteFile(poolPath, broken, 0600); err != nil {
		t.Fatalf("write: %v", err)
	}

	pool = nil
	p := loadPool()
	if len(p.Accounts) != 0 {
		t.Fatalf("应降级为空池，实际 %d 个账号", len(p.Accounts))
	}

	reason, _ := poolSaveBlocked.Load().(string)
	if reason == "" {
		t.Fatal("备份失败后必须设置保存拦截，否则后续写入会覆盖用户的账号文件")
	}
	if !strings.Contains(reason, "refusing to overwrite") {
		t.Fatalf("拦截原因应说明拒绝覆盖，实际：%s", reason)
	}

	// 关键：坏文件必须原样留在磁盘上，且任何保存都被拒绝
	after, err := os.ReadFile(poolPath)
	if err != nil {
		t.Fatalf("坏文件不该被删除: %v", err)
	}
	if string(after) != string(broken) {
		t.Fatalf("坏文件内容被改动了：%q", after)
	}
	if _, err := addCustomModel("a/one"); err == nil {
		t.Fatal("备份失败后保存必须被拒绝")
	}
	again, err := os.ReadFile(poolPath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(again) != string(broken) {
		t.Fatalf("文件在保存失败后仍被改写：%q → %q", broken, again)
	}
}
