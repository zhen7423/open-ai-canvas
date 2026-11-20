package protocol

import (
	"context"
	"strings"
	"testing"
)

func cangyuanAdapter(t *testing.T, provider string) Adapter {
	t.Helper()
	return officialPackageAdapter(t, "cangyuan-midjourney.yingce-plugin", provider)
}

func TestCangyuanMidjourneyV7RequestAndEditPolling(t *testing.T) {
	adapter := cangyuanAdapter(t, "cangyuan-midjourney-v7")
	request := GenerationRequest{Model: "midjourney-v7", Prompt: "生成一张氛围感美女照片", ImageCount: 1, AspectRatio: "auto", Quality: "high", Resolution: "2k", Extra: map[string]any{"transparentBackground": "true"}}
	spec, err := adapter.BuildCreate(context.Background(), RequestContext{Request: request})
	if err != nil {
		t.Fatal(err)
	}
	body := manifestTestBody(t, spec)
	if spec.Path != "/v1/images/generations" || body["n"] != float64(1) || body["response_format"] != "url" || body["async"] != false {
		t.Fatalf("create = %#v, body = %#v", spec, body)
	}
	for _, key := range []string{"size", "images", "quality", "resolution", "background", "speed", "output_format", "mask"} {
		if _, ok := body[key]; ok {
			t.Fatalf("unsupported field %s was emitted: %#v", key, body)
		}
	}
	request.Images = []MediaReference{{URL: "https://example.com/second.png", Role: "edit_source", Order: 2}, {URL: "https://example.com/first.png", Role: "edit_source", Order: 1}}
	request.AspectRatio = "9:16"
	spec, err = adapter.BuildCreate(context.Background(), RequestContext{Request: request})
	if err != nil {
		t.Fatal(err)
	}
	body = manifestTestBody(t, spec)
	images := body["images"].([]any)
	if spec.Path != "/v1/images/edits" || body["size"] != "9:16" || len(images) != 2 || images[0] != "https://example.com/first.png" {
		t.Fatalf("edit = %#v, body = %#v", spec, body)
	}
	poll, err := adapter.BuildPoll(context.Background(), PollContext{Request: request, TaskID: "task-42"})
	if err != nil || poll.Path != "/v1/images/edits/task-42" {
		t.Fatalf("edit poll = %#v, err = %v", poll, err)
	}
}

func TestCangyuanMidjourneyV82VersionsReferencesAndMask(t *testing.T) {
	adapter := cangyuanAdapter(t, "cangyuan-midjourney-v82")
	for _, model := range []string{"midjourney-1k", "midjourney-2k"} {
		request := GenerationRequest{Model: model, Prompt: "portrait --v 8.2", ImageCount: 1, Quality: "high"}
		spec, err := adapter.BuildCreate(context.Background(), RequestContext{Request: request})
		if err != nil {
			t.Fatal(err)
		}
		body := manifestTestBody(t, spec)
		if body["prompt"] != request.Prompt || body["model"] != model {
			t.Fatalf("version request = %#v", body)
		}
		for _, key := range []string{"quality", "resolution", "background", "output_format", "speed", "reference", "mask"} {
			if _, ok := body[key]; ok {
				t.Fatalf("unexpected %s in %#v", key, body)
			}
		}
	}
	request := GenerationRequest{Model: "midjourney-1k", Prompt: "portrait --v 8.2", Images: []MediaReference{{URL: "https://example.com/source.png", Role: "edit_source"}, {URL: "https://example.com/mask.png", Role: "mask"}}}
	spec, err := adapter.BuildCreate(context.Background(), RequestContext{Request: request})
	if err != nil {
		t.Fatal(err)
	}
	body := manifestTestBody(t, spec)
	if spec.Path != "/v1/images/edits" || body["reference"] != "editor" || body["mask"] != "https://example.com/mask.png" || len(body["images"].([]any)) != 1 {
		t.Fatalf("masked edit = %#v, body = %#v", spec, body)
	}
	request.Images = request.Images[:1]
	request.ProviderOptions = map[string]map[string]any{"cangyuan-midjourney-v82": {"reference": "style", "speed": "fast"}}
	spec, err = adapter.BuildCreate(context.Background(), RequestContext{Request: request})
	if err != nil {
		t.Fatal(err)
	}
	body = manifestTestBody(t, spec)
	if spec.Path != "/v1/images/generations" || body["reference"] != "style" || body["speed"] != "fast" {
		t.Fatalf("style = %#v, body = %#v", spec, body)
	}
}

func TestCangyuanMidjourneyV82WebLineSendsOfficialContract(t *testing.T) {
	adapter := cangyuanAdapter(t, "cangyuan-midjourney-v82-web")
	// 大小写不敏感校验，出站固定按供应商官网写法发送。
	request := GenerationRequest{Model: "midjourney-v8.2", Prompt: "portrait", ImageCount: 1, AspectRatio: "auto"}
	spec, err := adapter.BuildCreate(context.Background(), RequestContext{Request: request})
	if err != nil {
		t.Fatal(err)
	}
	body := manifestTestBody(t, spec)
	if spec.Path != "/v1/images/generations" || body["model"] != "Midjourney-v8.2" || body["prompt"] != request.Prompt || body["response_format"] != "url" || body["async"] != false {
		t.Fatalf("create = %#v, body = %#v", spec, body)
	}
	// n 与蒙版都在该线路的合同之外；size 为 auto 时不发送，未设置的扩展参数同样省略。
	for _, key := range []string{"n", "size", "images", "mask", "speed", "reference", "weird", "hd", "reference_slots", "quality", "resolution"} {
		if _, ok := body[key]; ok {
			t.Fatalf("unsupported field %s was emitted: %#v", key, body)
		}
	}
	poll, err := adapter.BuildPoll(context.Background(), PollContext{Request: request, TaskID: "task-42"})
	if err != nil || poll.Path != "/v1/images/generations/task-42" {
		t.Fatalf("poll = %#v, err = %v", poll, err)
	}

	// weird=0 与 hd=false 是用户显式选择的值，不能被当作空值丢弃。
	request.AspectRatio = "9:16"
	request.ProviderOptions = map[string]map[string]any{"cangyuan-midjourney-v82-web": {"weird": 0, "hd": false, "reference_slots": "content, Style "}}
	request.Images = []MediaReference{{URL: "https://example.com/second.png", Role: "edit_source", Order: 2}, {URL: "https://example.com/first.png", Role: "edit_source", Order: 1}}
	spec, err = adapter.BuildCreate(context.Background(), RequestContext{Request: request})
	if err != nil {
		t.Fatal(err)
	}
	body = manifestTestBody(t, spec)
	slots, _ := body["reference_slots"].([]any)
	if body["size"] != "9:16" || body["weird"] != float64(0) || body["hd"] != false || len(slots) != 2 || slots[0] != "content" || slots[1] != "style" {
		t.Fatalf("extended body = %#v", body)
	}
	images, _ := body["images"].([]any)
	if len(images) != 2 || images[0] != "https://example.com/first.png" {
		t.Fatalf("images = %#v", images)
	}
	if _, ok := body["n"]; ok {
		t.Fatalf("n must stay absent even with reference images: %#v", body)
	}
}

func TestCangyuanMidjourneyMJ2RequestAndPolling(t *testing.T) {
	adapter := cangyuanAdapter(t, "cangyuan-midjourney-mj2")
	// 按次计费线路：只发 model、prompt、n、size、images，质量、分辨率、速度、蒙版都不参与。
	request := GenerationRequest{Model: "mj2-midjourney", Prompt: "雨夜霓虹街景 --v 6.1 --style raw", ImageCount: 1, AspectRatio: "auto", Quality: "high", Resolution: "2k", Extra: map[string]any{"transparentBackground": "true"}}
	spec, err := adapter.BuildCreate(context.Background(), RequestContext{Request: request})
	if err != nil {
		t.Fatal(err)
	}
	body := manifestTestBody(t, spec)
	if spec.Path != "/v1/images/generations" || body["model"] != "mj2-midjourney" || body["n"] != float64(1) || body["response_format"] != "url" || body["async"] != false {
		t.Fatalf("create = %#v, body = %#v", spec, body)
	}
	for _, key := range []string{"size", "images", "quality", "resolution", "background", "speed", "output_format", "mask", "reference_slots", "hd", "weird"} {
		if _, ok := body[key]; ok {
			t.Fatalf("unsupported field %s was emitted: %#v", key, body)
		}
	}
	// 参考图仍走 generations（没有 edits 分支），按宿主顺序进入 images。
	request.AspectRatio = "3:2"
	request.Images = []MediaReference{{URL: "https://example.com/second.png", Role: "edit_source", Order: 2}, {URL: "https://example.com/first.png", Role: "edit_source", Order: 1}}
	spec, err = adapter.BuildCreate(context.Background(), RequestContext{Request: request})
	if err != nil {
		t.Fatal(err)
	}
	body = manifestTestBody(t, spec)
	images, _ := body["images"].([]any)
	if spec.Path != "/v1/images/generations" || body["size"] != "3:2" || len(images) != 2 || images[0] != "https://example.com/first.png" {
		t.Fatalf("reference create = %#v, body = %#v", spec, body)
	}
	poll, err := adapter.BuildPoll(context.Background(), PollContext{Request: request, TaskID: "task-42"})
	if err != nil || poll.Path != "/v1/images/generations/task-42" {
		t.Fatalf("poll = %#v, err = %v", poll, err)
	}
}

func TestCangyuanMidjourneyRejectsInvalidInputs(t *testing.T) {
	webOptions := func(value string) map[string]map[string]any {
		return map[string]map[string]any{"cangyuan-midjourney-v82-web": {"reference_slots": value}}
	}
	webImages := func(count int) []MediaReference {
		images := make([]MediaReference, 0, count)
		for index := 0; index < count; index++ {
			images = append(images, MediaReference{URL: "https://example.com/reference.png", Role: "edit_source", Order: index})
		}
		return images
	}
	for _, tc := range []struct {
		name, provider string
		request        GenerationRequest
	}{
		{"model mismatch", "cangyuan-midjourney-v7", GenerationRequest{Model: "midjourney-1k", Prompt: "portrait"}},
		{"invented v82 model", "cangyuan-midjourney-v82", GenerationRequest{Model: "midjourney-v8.2", Prompt: "portrait"}},
		{"multiple submissions", "cangyuan-midjourney-v7", GenerationRequest{Model: "midjourney-v7", Prompt: "portrait", ImageCount: 2}},
		{"long unicode prompt", "cangyuan-midjourney-v7", GenerationRequest{Model: "midjourney-v7", Prompt: strings.Repeat("画", 4001)}},
		{"data URI", "cangyuan-midjourney-v7", GenerationRequest{Model: "midjourney-v7", Prompt: "portrait", Images: []MediaReference{{DataURL: "data:image/png;base64,aA=="}}}},
		{"v7 mask", "cangyuan-midjourney-v7", GenerationRequest{Model: "midjourney-v7", Prompt: "portrait", Images: []MediaReference{{URL: "https://example.com/mask.png", Role: "mask"}}}},
		{"editor missing source", "cangyuan-midjourney-v82", GenerationRequest{Model: "midjourney-1k", Prompt: "portrait", Images: []MediaReference{{URL: "https://example.com/mask.png", Role: "mask"}}}},
		{"invalid speed", "cangyuan-midjourney-v82", GenerationRequest{Model: "midjourney-1k", Prompt: "portrait", ProviderOptions: map[string]map[string]any{"cangyuan-midjourney-v82": {"speed": "turbo"}}}},
		{"web line rejects 1k model", "cangyuan-midjourney-v82-web", GenerationRequest{Model: "midjourney-1k", Prompt: "portrait"}},
		{"web mask", "cangyuan-midjourney-v82-web", GenerationRequest{Model: "Midjourney-v8.2", Prompt: "portrait", Images: []MediaReference{{URL: "https://example.com/mask.png", Role: "mask"}}}},
		{"web prompt over 1024", "cangyuan-midjourney-v82-web", GenerationRequest{Model: "Midjourney-v8.2", Prompt: strings.Repeat("画", 1025)}},
		{"web over eight references", "cangyuan-midjourney-v82-web", GenerationRequest{Model: "Midjourney-v8.2", Prompt: "portrait", Images: webImages(9)}},
		{"web slots required above four", "cangyuan-midjourney-v82-web", GenerationRequest{Model: "Midjourney-v8.2", Prompt: "portrait", Images: webImages(5)}},
		{"web slots count mismatch", "cangyuan-midjourney-v82-web", GenerationRequest{Model: "Midjourney-v8.2", Prompt: "portrait", Images: webImages(2), ProviderOptions: webOptions("content")}},
		{"web slots unknown value", "cangyuan-midjourney-v82-web", GenerationRequest{Model: "Midjourney-v8.2", Prompt: "portrait", Images: webImages(2), ProviderOptions: webOptions("content,moodboard")}},
		{"mj2 line rejects v7 model", "cangyuan-midjourney-mj2", GenerationRequest{Model: "midjourney-v7", Prompt: "portrait"}},
		{"mj2 empty prompt", "cangyuan-midjourney-mj2", GenerationRequest{Model: "mj2-midjourney", Prompt: "   "}},
		{"mj2 multiple submissions", "cangyuan-midjourney-mj2", GenerationRequest{Model: "mj2-midjourney", Prompt: "portrait", ImageCount: 2}},
		{"mj2 data URI", "cangyuan-midjourney-mj2", GenerationRequest{Model: "mj2-midjourney", Prompt: "portrait", Images: []MediaReference{{DataURL: "data:image/png;base64,aA=="}}}},
		{"mj2 mask", "cangyuan-midjourney-mj2", GenerationRequest{Model: "mj2-midjourney", Prompt: "portrait", Images: []MediaReference{{URL: "https://example.com/mask.png", Role: "mask"}}}},
		{"mj2 over five references", "cangyuan-midjourney-mj2", GenerationRequest{Model: "mj2-midjourney", Prompt: "portrait", Images: webImages(6)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := cangyuanAdapter(t, tc.provider).BuildCreate(context.Background(), RequestContext{Request: tc.request}); err == nil {
				t.Fatal("invalid input was accepted")
			}
		})
	}
}

func TestCangyuanMidjourneyPreservesAllResultsAndTaskStates(t *testing.T) {
	for _, provider := range []string{"cangyuan-midjourney-v7", "cangyuan-midjourney-v82", "cangyuan-midjourney-v82-web", "cangyuan-midjourney-mj2"} {
		adapter := cangyuanAdapter(t, provider)
		for _, payload := range []string{`{"data":[{"url":"https://example.com/1.png"},{"url":"https://example.com/2.png"},{"url":"https://example.com/3.png"},{"url":"https://example.com/4.png"}]}`, `{"id":"task-42","status":"completed","data":[{"url":"https://example.com/1.png"},{"url":"https://example.com/2.png"},{"url":"https://example.com/3.png"},{"url":"https://example.com/4.png"}]}`} {
			result, err := adapter.ParseCreate(context.Background(), []byte(payload))
			if err != nil || result.Status != StatusSucceeded || result.Result == nil || len(result.Result.Images) != 4 {
				t.Fatalf("%s result = %#v, err = %v", provider, result, err)
			}
		}
		pending, err := adapter.ParseCreate(context.Background(), []byte(`{"id":"task-42","status":"queued"}`))
		if err != nil || pending.TaskID != "task-42" || pending.Status != StatusPending {
			t.Fatalf("pending = %#v, err = %v", pending, err)
		}
		failed, err := adapter.ParsePoll(context.Background(), PollContext{TaskID: "task-42"}, []byte(`{"status":"failed","error":{"code":"invalid_prompt","message":"prompt rejected"}}`))
		if err != nil || failed.TaskID != "task-42" || failed.Status != StatusFailed || failed.Message != "prompt rejected" {
			t.Fatalf("failure = %#v, err = %v", failed, err)
		}
	}
}
