// Package config 读服务器自身的部署配置。
//
// 只管"进程怎么起"：监听地址、端口、gin 模式。
// 数据库与 Redis 的连接参数不在这里——那是 Norm 的地盘，
// 由 data/orm.json 单独描述，orm.InitPool 自己去读。
// 两份配置分开是有意的：换数据库地址不该碰服务器配置，反之亦然。
package config

import (
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/bytedance/sonic"
)

// Server 对应 data/server.json。
type Server struct {
	Addr    string `json:"addr"`     // 监听地址，如 0.0.0.0
	Port    string `json:"port"`     // 监听端口，如 8080
	GinMode string `json:"gin_mode"` // debug / release / test，留空按 gin 默认

	// OpsToken 运维接口的令牌（邮件下发）。
	//
	// 它不在必填项里：留空时运维接口整组返回 503 而不是敞开，
	// 所以"忘了配"的后果是那组接口不可用，而不是任何人都能发道具。
	OpsToken string `json:"ops_token"`
}

// Listen 返回 net.Listen 用的 host:port。
func (that *Server) Listen() string { return net.JoinHostPort(that.Addr, that.Port) }

// LoadServer 读配置文件，随后用环境变量覆盖。
//
// 环境变量优先于文件，所以线上换端口不必改文件也不必重新打包：
//
//	SERVER_PORT=18080 ./noteserver
//
// 文件缺失不算错误——只要环境变量给全了就能起。这条让容器部署可以完全不带
// 配置文件，也让本地调试可以只改一个环境变量。
func LoadServer(path string) (*Server, error) {
	cfg := &Server{}

	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := sonic.Unmarshal(raw, cfg); err != nil {
			return nil, fmt.Errorf("解析 %s 失败: %w", path, err)
		}
	case os.IsNotExist(err):
		// 允许没有文件，下面用环境变量补
	default:
		return nil, fmt.Errorf("读取 %s 失败: %w", path, err)
	}

	overrideFromEnv("SERVER_ADDR", &cfg.Addr)
	overrideFromEnv("SERVER_PORT", &cfg.Port)
	overrideFromEnv("GIN_MODE", &cfg.GinMode)
	// 令牌尤其该走环境变量：写进入库的 json 等于把它公开了
	overrideFromEnv("OPS_TOKEN", &cfg.OpsToken)

	// 逐项报缺，别让人对着一句 "invalid config" 猜是哪个键没填
	missing := make([]string, 0, 2)
	if cfg.Addr == "" {
		missing = append(missing, "addr / SERVER_ADDR")
	}
	if cfg.Port == "" {
		missing = append(missing, "port / SERVER_PORT")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("%s 缺少必填项: %s", path, strings.Join(missing, ", "))
	}
	return cfg, nil
}

func overrideFromEnv(key string, dst *string) {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		*dst = v
	}
}
