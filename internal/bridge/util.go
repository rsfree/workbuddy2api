// util.go 翻译层共用的小工具。
package bridge

import "time"

// nowUnix 当前秒级时间戳（响应对象的 created_at/created 字段）。
// 单独封一层而不是各处 time.Now().Unix()：测试里替换更省事，也保证同一次
// 翻译内多个字段取到同一个时刻（客户端拿 created_at 做排序时不会跳）。
func nowUnix() int64 { return time.Now().Unix() }
