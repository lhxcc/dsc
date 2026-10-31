// Copyright 2015 rance
//
// Licensed under the Apache License, Version 2.0 (the "License"): you may
// not use this file except in compliance with the License. You may obtain
// a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS, WITHOUT
// WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the
// License for the specific language governing permissions and limitations
// under the License.
package cache

import (
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// tagIdSeq 仅在 crypto/rand 不可用时兜底，保证同一进程内不会重复。
var tagIdSeq uint64

type TagSet struct {
	store CacheStore
	names []string
}

func NewTagSet(store CacheStore, names []string) *TagSet {

	t := &TagSet{store, names}
	t.SetNames(names)
	return t
}

func (t *TagSet) SetNames(names []string) {
	t.names = append([]string(nil), names...)
}

// AddNames 追加 tag 名。去重且保留首次出现的顺序：命名空间是各 tag id 的拼接，
// 顺序必须稳定，否则同一组 tag 会算出不同命名空间（旧实现用 map 收敛，顺序随机）。
func (t *TagSet) AddNames(names []string) {
	if len(names) == 0 {
		return
	}

	merged := make([]string, 0, len(t.names)+len(names))
	seen := make(map[string]bool, len(t.names)+len(names))
	for _, group := range [][]string{t.names, names} {
		for _, name := range group {
			if seen[name] {
				continue
			}
			seen[name] = true
			merged = append(merged, name)
		}
	}

	t.names = merged
}

// 刷新所有 tag key
func (t *TagSet) Reset() error {
	for _, name := range t.names {
		t.ResetTag(name)
	}
	return nil
}

// 取tag id
func (t *TagSet) TagId(name string) string {
	var idstr string
	t.store.Get(t.TagKey(name), &idstr)

	if len(idstr) == 0 {
		return t.ResetTag(name)
	}

	return idstr
}

// 取所有的tagid
func (t *TagSet) TagIds() []string {
	l := len(t.names)
	if l == 0 {
		return []string{}
	}

	ids := make([]string, l)
	for i, name := range t.names {
		id := t.TagId(name)
		ids[i] = id
	}

	return ids
}

// 取命名空间
func (t *TagSet) GetNamespace() string {
	ids := t.TagIds()
	if len(ids) == 0 {
		return ""
	}
	return strings.Join(ids, "|")
}

// 重置key
func (t *TagSet) ResetTag(name string) string {
	id := t.generateId()
	err := t.store.Set(t.TagKey(name), id, 3600)
	if err != nil {
		panic(fmt.Errorf("ResetTag store Forever err : %v", err))
	}
	return id
}

// id标识算法：必须真正唯一，否则 ResetTag 换不掉命名空间（Flush 失效、旧值仍然命中）。
// 旧实现是「UnixNano + rand.Intn(9)」，而系统时钟只有节拍级精度——同一节拍内
// 300 次调用只得到 9 个不同值（Windows 实测），重复概率约 1/9。
func (t *TagSet) generateId() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err == nil {
		return hex.EncodeToString(buf[:])
	}

	// 熵源不可用：时间戳保底 + 进程内自增，仍不会重复。
	return strconv.FormatInt(time.Now().UnixNano(), 10) + "-" +
		strconv.FormatUint(atomic.AddUint64(&tagIdSeq, 1), 10)
}

// Tag key
func (t *TagSet) TagKey(name string) string {
	return fmt.Sprintf("tag:%s:key", name)
}

func EncodeSha1(str string) string {
	h := sha1.New()
	h.Write([]byte(str))
	return hex.EncodeToString(h.Sum(nil))
}
