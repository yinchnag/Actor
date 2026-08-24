-- noteserver 建表语句
--
--   mysql -u root -p < cmd/noteserver/schema.sql
--
-- 字符集必须是 utf8mb4。MySQL 里的 utf8 是残废的三字节实现，存不下 emoji
-- 和部分生僻汉字，笔记这种自由文本一定要用 utf8mb4。

-- 排序规则用 utf8mb4_unicode_ci 而不是 utf8mb4_0900_ai_ci：
-- 后者是 MySQL 8.0 才有的，5.7 上会直接报 Unknown collation。
CREATE DATABASE IF NOT EXISTS notebook
  DEFAULT CHARACTER SET utf8mb4
  DEFAULT COLLATE utf8mb4_unicode_ci;

USE notebook;

CREATE TABLE IF NOT EXISTS users (
  id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  -- 手机号定长 11 位，用 CHAR 而不是 VARCHAR。
  -- 排序规则单独指定为 utf8mb4_bin：手机号只有数字，不需要大小写/重音折叠，
  -- 二进制比较更快，也不会因为库的默认排序规则变化而影响唯一性判定。
  phone         CHAR(11) COLLATE utf8mb4_bin NOT NULL,
  -- bcrypt 输出固定 60 字节的 ASCII，留到 72 是给将来换算法（如 argon2id）的余量
  password_hash VARCHAR(72) COLLATE utf8mb4_bin NOT NULL,
  created_at    DATETIME(3) NOT NULL,
  PRIMARY KEY (id),
  -- 唯一索引是多实例部署时的最后一道防线。
  -- 单实例下注册已经按手机号分片串行化了（见 hub.go），但不同进程的分片
  -- 各自独立，只有数据库能保证全局唯一。
  UNIQUE KEY uk_phone (phone)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS notes (
  id         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  user_id    BIGINT UNSIGNED NOT NULL,
  -- 笔记没有标题，就是一段文字。
  -- 需求是至少能存 800 汉字：utf8mb4 下 800 汉字 = 2400 字节。
  -- 这里用 MEDIUMTEXT（16MB）而不是 TEXT（64KB），是为了让"按字数限制"这件事
  -- 不必再操心字节数——程序里限 20000 字，即使全是 4 字节字符也才 80KB，
  -- 用 TEXT 反而要额外提防字节溢出被静默截断。
  content    MEDIUMTEXT NOT NULL,
  -- 上传日期。DATETIME(3) 与程序里 Truncate(time.Millisecond) 精度对齐，
  -- 否则返回给客户端的时间和库里存的会差一点。
  created_at DATETIME(3) NOT NULL,
  PRIMARY KEY (id),
  -- 覆盖"按用户取最新 N 条"这个唯一的查询模式。
  -- 带上 id 是为了同一毫秒内上传的多条也有确定顺序。
  -- 不写 DESC：降序索引是 MySQL 8.0 的特性，5.7 只会静默忽略这个关键字，
  -- 写了容易让人误以为真建了降序索引。倒序扫这个索引对两者都一样高效。
  KEY idx_user_created (user_id, created_at, id),
  CONSTRAINT fk_notes_user FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
