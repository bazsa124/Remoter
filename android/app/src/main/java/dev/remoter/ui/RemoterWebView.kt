package dev.remoter.ui

import android.content.Context
import android.view.inputmethod.EditorInfo
import android.view.inputmethod.InputConnection
import android.webkit.WebView

/**
 * A WebView that keeps text input working in landscape.
 *
 * In landscape Android normally switches the IME to "extract" mode: a full-screen
 * editor replaces the page, and what you type goes to that editor rather than to
 * the field underneath. For a normal form you get the text back on commit; for a
 * page that reads every keystroke as it is typed - which is exactly how Tier 3
 * forwards characters to the host - the keystrokes never arrive, and the keyboard
 * looks broken.
 *
 * Asking the IME not to take over the screen fixes it, and there is no way to do
 * that from inside the page.
 */
class RemoterWebView(context: Context) : WebView(context) {

	override fun onCreateInputConnection(outAttrs: EditorInfo): InputConnection? {
		val connection = super.onCreateInputConnection(outAttrs)
		outAttrs.imeOptions = outAttrs.imeOptions or
			EditorInfo.IME_FLAG_NO_EXTRACT_UI or
			EditorInfo.IME_FLAG_NO_FULLSCREEN
		return connection
	}
}
