package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/redis/go-redis/v9"
)

var (
	// ErrPhoneTaken 手机号已注册。
	ErrPhoneTaken = errors.New("手机号已被注册")
	// ErrUserNotFound 手机号没有对应的账号。
	ErrUserNotFound = errors.New("账号不存在")
	// ErrNoSession token 不存在或已过期。
	ErrNoSession = errors.New("会话不存在或已过期")
)

// User 是 users 表的一行。
type User struct {
	ID           int64
	Phone        string
	PasswordHash string
}

// Note 是一条笔记。没有标题，就是一段文字加上传时间。
type Note struct {
	ID        int64     `json:"id"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
}

// Store 是持久化存储的抽象。抽出接口不是为了将来换数据库，
// 而是为了让 actor 编排和 HTTP 层能在没有 MySQL 的环境里被完整测到——
// 测试里换成内存实现即可，见 server_test.go。
type Store interface {
	CreateUser(ctx context.Context, phone, passwordHash string) (int64, error)
	FindUserByPhone(ctx context.Context, phone string) (*User, error)
	InsertNote(ctx context.Context, userID int64, content string, createdAt time.Time) (int64, error)
	ListNotes(ctx context.Context, userID int64, limit int) ([]Note, error)
	Close() error
}

// SessionStore 存放登录会话。放 Redis 而不是进程内存，
// 是为了多实例部署时会话能共享，且进程重启不掉线。
type SessionStore interface {
	Put(ctx context.Context, token string, userID int64, ttl time.Duration) error
	Get(ctx context.Context, token string) (int64, error)
	Delete(ctx context.Context, token string) error
	Close() error
}

// --- MySQL ---

type mysqlStore struct{ db *sql.DB }

// OpenMySQL 建库连接。
//
// 连接池上限刻意压得比较低：本服务的并发瓶颈在 actor 事件循环而不是数据库，
// 而每个查询都跑在某个 actor 的事件循环里（会把那个 actor 钉住），
// 池子开太大只会让数据库先被打爆。
func OpenMySQL(dsn string) (Store, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开 MySQL 失败: %w", err)
	}
	db.SetMaxOpenConns(32)
	db.SetMaxIdleConns(8)
	db.SetConnMaxLifetime(30 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("连接 MySQL 失败: %w", err)
	}
	return &mysqlStore{db: db}, nil
}

func (s *mysqlStore) Close() error { return s.db.Close() }

func (s *mysqlStore) CreateUser(ctx context.Context, phone, passwordHash string) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO users (phone, password_hash, created_at) VALUES (?, ?, ?)`,
		phone, passwordHash, time.Now())
	if err != nil {
		// 即使注册已经按手机号分片串行化了，唯一索引也得留着：
		// 多实例部署时不同进程的分片是各自独立的，最终一致性只能靠数据库兜。
		var me *mysql.MySQLError
		if errors.As(err, &me) && me.Number == 1062 {
			return 0, ErrPhoneTaken
		}
		return 0, err
	}
	return res.LastInsertId()
}

func (s *mysqlStore) FindUserByPhone(ctx context.Context, phone string) (*User, error) {
	var u User
	err := s.db.QueryRowContext(ctx,
		`SELECT id, phone, password_hash FROM users WHERE phone = ?`, phone).
		Scan(&u.ID, &u.Phone, &u.PasswordHash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

func (s *mysqlStore) InsertNote(ctx context.Context, userID int64, content string, createdAt time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO notes (user_id, content, created_at) VALUES (?, ?, ?)`,
		userID, content, createdAt)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *mysqlStore) ListNotes(ctx context.Context, userID int64, limit int) ([]Note, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, content, created_at FROM notes
		 WHERE user_id = ? ORDER BY created_at DESC, id DESC LIMIT ?`,
		userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	notes := make([]Note, 0, 16)
	for rows.Next() {
		var n Note
		if err := rows.Scan(&n.ID, &n.Content, &n.CreatedAt); err != nil {
			return nil, err
		}
		notes = append(notes, n)
	}
	return notes, rows.Err()
}

// --- Redis ---

type redisSessions struct{ cli *redis.Client }

func OpenRedis(url string) (SessionStore, error) {
	opt, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("解析 REDIS_URL 失败: %w", err)
	}
	cli := redis.NewClient(opt)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := cli.Ping(ctx).Err(); err != nil {
		cli.Close()
		return nil, fmt.Errorf("连接 Redis 失败: %w", err)
	}
	return &redisSessions{cli: cli}, nil
}

func (s *redisSessions) Close() error { return s.cli.Close() }

func sessionKey(token string) string { return "sess:" + token }

func (s *redisSessions) Put(ctx context.Context, token string, userID int64, ttl time.Duration) error {
	return s.cli.Set(ctx, sessionKey(token), userID, ttl).Err()
}

func (s *redisSessions) Get(ctx context.Context, token string) (int64, error) {
	id, err := s.cli.Get(ctx, sessionKey(token)).Int64()
	if errors.Is(err, redis.Nil) {
		return 0, ErrNoSession
	}
	if err != nil {
		return 0, err
	}
	return id, nil
}

func (s *redisSessions) Delete(ctx context.Context, token string) error {
	return s.cli.Del(ctx, sessionKey(token)).Err()
}
