// tako-dialogue-eval 是历史对话混合检索活体验收（doc/84，scripts/acceptance/dialogue_hybrid_*.sh）
// 专用的 Milvus 体检小工具。验收 collection 用时间戳名（tako_dialogue_eval_*），验收结束由
// 脚本用本工具删除——只删自己创建的，不碰共享 demo collection。
//
// 为什么不用 REST：Zilliz Cloud 的 v2 REST 对 describe/query/drop 返回 403 PrivilegeDenied
// （P4 实测），只有 has/list 可用；所以计数、删行、删 collection 必须走引擎同款官方 SDK。
//
// 子命令（全部从 TAKO_MILVUS_URI / TAKO_MILVUS_TOKEN / TAKO_MILVUS_DB / TAKO_MILVUS_USE_TLS 取连接参数）：
//
//	tako-dialogue-eval has -collection X                    → stdout: true|false
//	tako-dialogue-eval count -collection X [-expr "..."]    → stdout: 行数（整数）
//	                                                          -expr 传 Milvus 布尔表达式（如 "index_version == 1"），
//	                                                          缺省 "index_version >= 0"（全量）
//	tako-dialogue-eval drop -collection X                   → 删除 collection
//	tako-dialogue-eval delete -collection X -run-ids a,b,c  → 按 run_id 主键删行（场景 4 的 Milvus 零命中注入用）
//	tako-dialogue-eval create-baddim -collection X          → 造一个 8 维最小 collection（场景 6 的
//	                                                          启动预检维度不符：引擎 EnsureCollection 比对
//	                                                          维度不符必须拒起；验收后 drop 清理）
//
// 退出码：0 成功；2 调用失败（错误打到 stderr）。Zilliz serverless 冷启动超时重试 2 次。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/milvus-io/milvus-sdk-go/v2/client"
	"github.com/milvus-io/milvus-sdk-go/v2/entity"
)

func newClient() (client.Client, error) {
	cfg := client.Config{
		Address:       os.Getenv("TAKO_MILVUS_URI"),
		APIKey:        os.Getenv("TAKO_MILVUS_TOKEN"),
		DBName:        os.Getenv("TAKO_MILVUS_DB"),
		EnableTLSAuth: os.Getenv("TAKO_MILVUS_USE_TLS") == "1",
	}
	if cfg.Address == "" {
		return nil, fmt.Errorf("TAKO_MILVUS_URI 为空")
	}
	return client.NewClient(context.Background(), cfg)
}

// doTransient：Zilliz serverless 冷启动（DeadlineExceeded / 瞬时错误）重试 2 次，共 3 次尝试。
func doTransient(name string, fn func() error) error {
	var err error
	for i := 0; i < 3; i++ {
		if i > 0 {
			time.Sleep(time.Duration(i*2) * time.Second)
		}
		if err = fn(); err == nil {
			return nil
		}
	}
	return fmt.Errorf("%s: %w", name, err)
}

func die(err error) {
	fmt.Fprintln(os.Stderr, "tako-dialogue-eval:", err)
	os.Exit(2)
}

// countRows 按 expr 分页数行（与引擎 ListAllRunIDs 同款 limit/offset 分页，
// 避开 serverless 对大 offset 的限制）。
func countRows(ctx context.Context, c client.Client, coll, expr string) (int64, error) {
	const page = 500
	var total int64
	for offset := 0; ; offset += page {
		var rs client.ResultSet
		opts := []client.SearchQueryOptionFunc{client.WithLimit(page), client.WithOffset(int64(offset))}
		if err := doTransient("query", func() error {
			var qerr error
			rs, qerr = c.Query(ctx, coll, []string{}, expr, []string{"run_id"}, opts...)
			return qerr
		}); err != nil {
			return 0, err
		}
		n := 0
		if len(rs) > 0 {
			for _, col := range rs {
				if col.Name() == "run_id" {
					n = col.Len()
					break
				}
			}
			if n == 0 {
				n = rs[0].Len()
			}
		}
		total += int64(n)
		if n < page {
			break
		}
	}
	return total, nil
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "用法: tako-dialogue-eval has|count|drop|delete -collection X ...")
		os.Exit(2)
	}
	cmd := os.Args[1]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	coll := fs.String("collection", "", "collection 名")
	expr := fs.String("expr", "index_version >= 0", "count 的过滤表达式")
	runIDs := fs.String("run-ids", "", "delete 用，逗号分隔的 run_id")
	_ = fs.Parse(os.Args[2:])
	if *coll == "" {
		die(fmt.Errorf("缺少 -collection"))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	c, err := newClient()
	if err != nil {
		die(err)
	}
	defer c.Close()

	exists := false
	if err := doTransient("has", func() error {
		e, herr := c.HasCollection(ctx, *coll)
		exists = e
		return herr
	}); err != nil {
		die(err)
	}

	switch cmd {
	case "has":
		fmt.Println(exists)
	case "count":
		if !exists {
			fmt.Println(0)
			return
		}
		// 先 flush（best-effort），让删行/写入对后续 Query 尽快可见。
		_ = c.Flush(ctx, *coll, false)
		n, cerr := countRows(ctx, c, *coll, *expr)
		if cerr != nil {
			die(cerr)
		}
		fmt.Println(n)
	case "drop":
		if !exists {
			fmt.Println("absent")
			return
		}
		if err := doTransient("drop", func() error { return c.DropCollection(ctx, *coll) }); err != nil {
			die(err)
		}
		fmt.Println("dropped")
	case "create-baddim":
		if exists {
			fmt.Println("exists")
			return
		}
		schema := entity.NewSchema().
			WithField(entity.NewField().WithName("run_id").WithDataType(entity.FieldTypeVarChar).
				WithIsPrimaryKey(true).WithMaxLength(64)).
			WithField(entity.NewField().WithName("embedding").WithDataType(entity.FieldTypeFloatVector).WithDim(8))
		// Entity.NewSchema 需要集合名；直接带上 collection 名。
		schema = schema.WithName(*coll)
		if err := doTransient("create-baddim", func() error {
			return c.CreateCollection(ctx, schema, 1)
		}); err != nil {
			die(err)
		}
		fmt.Println("created")
	case "delete":
		if !exists {
			fmt.Println("absent")
			return
		}
		ids := strings.Split(*runIDs, ",")
		clean := make([]string, 0, len(ids))
		for _, id := range ids {
			id = strings.TrimSpace(id)
			if id != "" {
				clean = append(clean, id)
			}
		}
		if len(clean) == 0 {
			die(fmt.Errorf("缺少 -run-ids"))
		}
		if err := doTransient("delete", func() error {
			return c.DeleteByPks(ctx, *coll, "", entity.NewColumnVarChar("run_id", clean))
		}); err != nil {
			die(err)
		}
		_ = c.Flush(ctx, *coll, false)
		fmt.Println("deleted " + strconv.Itoa(len(clean)))
	default:
		die(fmt.Errorf("未知子命令 %s", cmd))
	}
}
