-- noteserver 建库语句
--
--   mysql -uroot -p < cmd/noteserver/schema.sql
--
-- 只建库，不建表。表由 Norm 的 AutoMigrate 在进程启动时创建并补列
-- （databases 包里每个 store 的构造函数都会触发一次），
-- 所以改字段只要改 Go 结构体上的 orm tag，不必回来改这个文件。
--
-- 字符集必须是 utf8mb4。MySQL 里的 utf8 是残废的三字节实现，存不下 emoji
-- 和部分生僻汉字，笔记这种自由文本一定要用 utf8mb4。
--
-- 排序规则用 utf8mb4_unicode_ci 而不是 utf8mb4_0900_ai_ci：
-- 后者是 MySQL 8.0 才有的，5.7 上会直接报 Unknown collation。
CREATE DATABASE IF NOT EXISTS notebook
  DEFAULT CHARACTER SET utf8mb4
  DEFAULT COLLATE utf8mb4_unicode_ci;
