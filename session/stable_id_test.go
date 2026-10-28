package session

import "testing"

// TestDerivedMessagesCarryStableIDs 派生消息必须带稳定 id（= surface 事件的 seq）。
//
// 这是压缩后端等「跨请求指认某条消息」的组件唯一的可靠身份：它与派生范围无关，
// 因此请求级裁剪、history_injection 只带最近 N 条、surface replace 遮蔽之后，
// 同一条消息的 id 都不变。曾用列表下标当身份，裁剪一发生就整体错位，压缩块遮错内容。
func TestDerivedMessagesCarryStableIDs(t *testing.T) {
	s := New()
	s.Append(SystemMessage, &SystemMessageData{Content: "sys"}, &SurfaceOp{Op: SurfaceAppend})
	for i := 0; i < 5; i++ {
		s.Append(UserMessage, &UserMessageData{Content: "u" + string(rune('0'+i))}, &SurfaceOp{Op: SurfaceAppend})
		s.Append(AssistantMessage, &AssistantMessageData{Content: "a" + string(rune('0'+i))}, &SurfaceOp{Op: SurfaceAppend})
	}

	full := s.DeriveMessages()
	if len(full) != 11 {
		t.Fatalf("派生条数 = %d, 期望 11", len(full))
	}
	seen := map[string]bool{}
	for i, m := range full {
		if m.Id == "" {
			t.Fatalf("第 %d 条消息没有稳定 id（role=%s）", i, m.Role)
		}
		if seen[m.Id] {
			t.Fatalf("稳定 id 重复：%s", m.Id)
		}
		seen[m.Id] = true
	}

	// 二次派生：同一条消息必须拿到同一个 id（派生是纯投影，不得每次重编号）
	again := s.DeriveMessages()
	for i := range full {
		if full[i].Id != again[i].Id {
			t.Fatalf("重复派生 id 漂移：第 %d 条 %q -> %q", i, full[i].Id, again[i].Id)
		}
	}
}

// TestStableIDsSurviveHeadTrimming 列表被截短（请求级裁剪 / 只注入最近 N 条）时，
// 剩余消息的 id 必须与全量派生时**完全一致**——这正是「按位置寻址」办不到的那件事。
func TestStableIDsSurviveHeadTrimming(t *testing.T) {
	s := New()
	s.Append(SystemMessage, &SystemMessageData{Content: "sys"}, &SurfaceOp{Op: SurfaceAppend})
	for i := 0; i < 6; i++ {
		s.Append(UserMessage, &UserMessageData{Content: "u" + string(rune('0'+i))}, &SurfaceOp{Op: SurfaceAppend})
		s.Append(AssistantMessage, &AssistantMessageData{Content: "a" + string(rune('0'+i))}, &SurfaceOp{Op: SurfaceAppend})
	}

	full := s.DeriveMessages()
	byContent := map[string]string{}
	for _, m := range full {
		byContent[m.Content] = m.Id
	}

	// 模拟宿主裁剪：只从第 4 条（u1）开始发
	trimmed := s.DeriveMessages()[4:]
	for _, m := range trimmed {
		if want := byContent[m.Content]; want != m.Id {
			t.Fatalf("裁剪后 %q 的 id = %q，全量时为 %q（位置寻址会在这里整体错位）",
				m.Content, m.Id, want)
		}
	}
}

// TestStableIDsUnchangedBySurfaceReplace surface replace（压缩摘要遮蔽前缀）只改变
// 派生出哪些消息，不改变被保留消息的 id。
func TestStableIDsUnchangedBySurfaceReplace(t *testing.T) {
	s := New()
	s.Append(SystemMessage, &SystemMessageData{Content: "sys"}, &SurfaceOp{Op: SurfaceAppend})
	for i := 0; i < 4; i++ {
		s.Append(UserMessage, &UserMessageData{Content: "u" + string(rune('0'+i))}, &SurfaceOp{Op: SurfaceAppend})
	}
	before := map[string]string{}
	for _, m := range s.DeriveMessages() {
		before[m.Content] = m.Id
	}

	// 用摘要替换 [1,3]（不碰 system node 0）
	s.Append(CompactionSummary, &CompactionSummaryData{Content: "摘要"},
		&SurfaceOp{Op: SurfaceReplace, Start: 1, End: 3})

	after := s.DeriveMessages()
	for _, m := range after {
		if m.Content == "摘要" {
			if m.Id == "" {
				t.Fatal("替换产生的摘要消息也应有稳定 id")
			}
			continue
		}
		if want, ok := before[m.Content]; ok && want != m.Id {
			t.Fatalf("replace 后 %q 的 id = %q，之前为 %q", m.Content, m.Id, want)
		}
	}
}
