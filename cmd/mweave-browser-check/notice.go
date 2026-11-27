package main

import (
	"net/url"
	"slices"
	"strconv"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

// The script reports fixed notice kinds from the observed page. Text is
// classified inside the browser and never enters the report or the CDP binding.
const pageNoticeScript = `(entry => {
	if (window !== window.top || location.origin !== new URL(entry).origin) return;
	const live = '[role="alert"],[role="status"],[aria-live="polite"],[aria-live="assertive"]';
	const choices = 'button,a,[role="button"],[role="menuitem"]';
	const pane = '[role="dialog"],[aria-modal="true"],dialog,[role="menu"]';
	const downloadChoices = new Map([
		["普通下载","ordinary_download_option"],["继续普通下载","ordinary_download_option"],
		["下载原文件","original_download_option"],["下载原图","original_download_option"],
		["客户端下载","desktop_download_option"],["打开客户端","desktop_download_option"]
	]);
	const operations = new Map([["上传","upload"],["移动","move"],["重命名","rename"],["删除","delete"],["复制","copy"],["操作","operation"]]);
	const previous = new WeakMap();
	function inspect(node) {
		if (!node || !node.isConnected || node.closest('[aria-hidden="true"],[aria-live="off"],[hidden]')) return;
		const text = node.textContent.trim();
		// Remember only the previous classified notice, not arbitrary page text.
		const match = /^(上传|移动|重命名|删除|复制|操作)(成功|失败|完成)[。！!.]?$/.exec(text);
		const kind = node.matches(choices) ? downloadChoices.get(node.getAttribute("aria-label")?.trim() || text) :
			text === "文件已删除到回收站" ? "delete_success" : match ? operations.get(match[1]) + "_" + ({成功:"success",失败:"failure",完成:"complete"})[match[2]] : "";
		if (previous.get(node) === kind) return;
		if (kind && (!node.getClientRects().length || getComputedStyle(node).visibility !== "visible")) return;
		previous.set(node, kind);
		if (kind) mweavePageNotice(kind);
	}
	function inspectContainer(container) {
		if (!container) return;
		for (const choice of Array.from(container.querySelectorAll(choices)).slice(0, 16)) inspect(choice);
	}
	new MutationObserver(changes => {
		for (const change of changes.slice(0, 64)) {
			const element = change.target.nodeType === Node.ELEMENT_NODE ? change.target : change.target.parentElement;
			inspect(element?.closest(live));
			inspectContainer(element?.closest(pane));
			for (const added of Array.from(change.addedNodes).slice(0, 16)) {
				if (added.nodeType !== Node.ELEMENT_NODE) continue;
				if (added.matches(live)) inspect(added);
				// A toast portal may insert its live-region element under a shell.
				inspect(added.querySelector(live));
				inspectContainer(added.matches(pane) ? added : added.querySelector(pane));
			}
		}
	}).observe(document, {childList: true, characterData: true, subtree: true, attributes:true, attributeFilter:["hidden","open","aria-hidden"]});
})`

func (r *recording) observePageNotices(entry *url.URL) {
	const binding = "mweavePageNotice"
	const world = "mweave-notice"
	if _, err := chromedp.Call(r.ctx, runtime.AddBinding, runtime.AddBindingParams{
		Name: binding, ExecutionContextName: world,
	}); err != nil {
		return
	}
	source := pageNoticeScript + "(" + strconv.Quote(requestOrigin(entry)) + ")"
	if _, err := chromedp.Call(r.ctx, page.AddScriptToEvaluateOnNewDocument, page.AddScriptToEvaluateOnNewDocumentParams{
		Source: source, WorldName: world, RunImmediately: new(true),
	}); err != nil {
		return
	}
	recordEvents(r, chromedp.Events(r.listen, runtime.BindingCalled), func(phase *recordedPhase) *recentEvents[string] {
		return &phase.notices
	}, func(event runtime.EventBindingCalled) (string, bool) {
		if event.Name != binding {
			return "", false
		}
		return event.Payload, slices.Contains([]string{
			"ordinary_download_option", "original_download_option", "desktop_download_option",
			"upload_success", "upload_failure", "upload_complete",
			"move_success", "move_failure", "move_complete",
			"rename_success", "rename_failure", "rename_complete",
			"delete_success", "delete_failure", "delete_complete",
			"copy_success", "copy_failure", "copy_complete",
			"operation_success", "operation_failure", "operation_complete",
		}, event.Payload)
	})
}
