package app

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// 造一个最小 MP4：ftyp + uuid + moov + free + mdat，尾部再挂一段"信封"。
func fakeMP4(payload []byte) []byte {
	var b bytes.Buffer
	box := func(typ string, data []byte) {
		var sz [4]byte
		binary.BigEndian.PutUint32(sz[:], uint32(8+len(data)))
		b.Write(sz[:])
		b.WriteString(typ)
		b.Write(data)
	}
	box("ftyp", bytes.Repeat([]byte("x"), 24))
	box("uuid", bytes.Repeat([]byte("y"), 16))
	box("moov", bytes.Repeat([]byte("z"), 32))
	box("free", []byte{0, 0, 0, 0})
	box("mdat", payload)
	return b.Bytes()
}

// protobuf 长度前缀编码
func pbLen(n int) []byte {
	var out []byte
	for {
		b := byte(n & 0x7f)
		n >>= 7
		if n != 0 {
			b |= 0x80
		}
		out = append(out, b)
		if n == 0 {
			return out
		}
	}
}

// 视频下载返回的是 protobuf 信封：外层 field1 包着内层
// field1{ field1:"video/mp4", field2:<4字节长度前缀+MP4> }。
// 不剥的话客户端拿到的是坏文件（2026-10-23 实测 4.9MB 的 "data"）。
func TestUnwrapMediaEnvelope(t *testing.T) {
	mp4 := fakeMP4([]byte("REAL VIDEO BYTES"))

	inner := append([]byte{0x0a, 0x09}, []byte("video/mp4")...) // field1 = mime
	field2 := append(pbLen(len(mp4)+4), mp4...)               // field2 = 4字节长度前缀 + MP4
	inner = append(inner, 0x12)
	inner = append(inner, pbLen(len(field2))...)
	inner = append(inner, field2...)

	outer := append([]byte{0x0a}, pbLen(len(inner))...)
	outer = append(outer, inner...)
	outer = append(outer, []byte("\n\x04tail")...) // 信封尾部

	out, mime := unwrapMediaEnvelope(outer)
	if mime != "video/mp4" {
		t.Errorf("mime = %q, 期望 video/mp4", mime)
	}
	if !bytes.Equal(out, mp4) {
		t.Errorf("解出的字节不对：%d 字节，期望 %d 字节", len(out), len(mp4))
	}
}

// 本身就是 MP4（没有信封）时必须原样返回，不能瞎剥。
func TestUnwrapMediaEnvelopePassthrough(t *testing.T) {
	mp4 := fakeMP4([]byte("REAL VIDEO BYTES"))
	out, mime := unwrapMediaEnvelope(mp4)
	if !bytes.Equal(out, mp4) {
		t.Error("裸 MP4 被改动了")
	}
	if mime != "" {
		t.Errorf("裸 MP4 不该带 mime，拿到 %q", mime)
	}
}

// 媒体模型的成功判据：视频提交的确认帧没有 inner[4] 正文，
// hasContentFrame 必然误判成「上游空响应」（2026-10-23 实测把成功的提交报成 502）。
func TestHasMediaAck(t *testing.T) {
	// 真实抓到的视频确认帧（结构：inner 长度 3，正文在 "7" 字段里）
	ack := `[["wrb.fr",null,"[null,[\"c_abc\",\"r_def\"],{\"7\":[\"I'm generating your video. ` +
		`This could take a few minutes, so check back to see when your video is ready.\",` +
		`[\"video_generation\",[null,null,\"Video Generation\",\"https://www.gstatic.com/x.svg\"],1,` +
		`[null,[null,null,null,null,null,null,null,null,[1]]]]],\"44\":true}]"]]`
	if !hasMediaAck(ack) {
		t.Error("视频确认帧应该算作有实质响应")
	}
	// 被拒绝的响应：只有一个结束帧，没有 wrb.fr
	if hasMediaAck(`[["e",7,null,null,948]]`) {
		t.Error("只有结束帧（被拒）不该算作有实质响应")
	}
}
