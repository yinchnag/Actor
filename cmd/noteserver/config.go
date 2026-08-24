package main

import (
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/joho/godotenv"
)

// Config 是全部外部配置，一一对应 .env 里的四个键。
//
// .env 只放"部署环境相关"的东西：监听地址端口、两个存储的连接串。
// 其余参数（会话有效期、空闲回收间隔、笔记长度上限等）都是程序行为，
// 写在代码常量里，不进 .env——配置项越少越不容易配错。
type Config struct {
	ServerAddr string // 监听地址，如 0.0.0.0
	ServerPort string // 监听端口，如 8080
	MySQLDSN   string // go-sql-driver/mysql 的 DSN
	RedisURL   string // redis://[user:pass@]host:port/db
}

// Listen 返回 net.Listen 用的 host:port。
func (c *Config) Listen() string {
	return net.JoinHostPort(c.ServerAddr, c.ServerPort)
}

// LoadConfig 读取 .env 并校验。
//
// godotenv.Load 不会覆盖已存在的环境变量，所以线上可以直接用真实环境变量
// 盖掉 .env 里的值，不必改文件；.env 文件本身缺失也不算错误——
// 只要环境变量给全了就行。
func LoadConfig(path string) (*Config, error) {
	if err := godotenv.Load(path); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("读取 %s 失败: %w", path, err)
	}

	cfg := &Config{
		ServerAddr: strings.TrimSpace(os.Getenv("SERVER_ADDR")),
		ServerPort: strings.TrimSpace(os.Getenv("SERVER_PORT")),
		MySQLDSN:   strings.TrimSpace(os.Getenv("MYSQL_DSN")),
		RedisURL:   strings.TrimSpace(os.Getenv("REDIS_URL")),
	}

	// 逐项报缺，别让人对着一句 "invalid config" 猜是哪个键没填
	missing := make([]string, 0, 4)
	for k, v := range map[string]string{
		"SERVER_ADDR": cfg.ServerAddr,
		"SERVER_PORT": cfg.ServerPort,
		"MYSQL_DSN":   cfg.MySQLDSN,
		"REDIS_URL":   cfg.RedisURL,
	} {
		if v == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("%s 缺少必填项: %s", path, strings.Join(missing, ", "))
	}
	return cfg, nil
}
