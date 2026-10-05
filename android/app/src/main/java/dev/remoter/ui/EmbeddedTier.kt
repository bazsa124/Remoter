package dev.remoter.ui

import android.annotation.SuppressLint
import android.net.Uri
import android.view.ViewGroup
import android.webkit.JavascriptInterface
import android.webkit.WebChromeClient
import android.webkit.WebResourceError
import android.webkit.WebResourceRequest
import android.webkit.WebSettings
import android.webkit.WebView
import android.webkit.WebViewClient
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.padding
import androidx.compose.material3.Button
import androidx.compose.material3.Text
import androidx.compose.runtime.*
import androidx.compose.ui.Modifier
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.compose.ui.viewinterop.AndroidView
import kotlinx.coroutines.delay

/**
 * Hosts one of the hub's own pages inside the app.
 *
 * Used for Console and Live, which are renderers rather than data - a terminal
 * emulator and a JPEG tile compositor - and for the Access page. Reimplementing
 * either renderer natively would mean reproducing behaviour that already works,
 * and getting it subtly wrong.
 *
 * Nothing is handed to the page: the hub knows this phone by its tailnet
 * identity, so the WebView needs no token, and its requests carry the same PIN
 * grant the native screens just obtained.
 */
@SuppressLint("SetJavaScriptEnabled", "JavascriptInterface")
@Composable
fun EmbeddedPage(
	url: String,
	onFullscreen: (Boolean) -> Unit = {},
	modifier: Modifier = Modifier,
) {
	var error by remember { mutableStateOf<String?>(null) }
	var reloadKey by remember { mutableIntStateOf(0) }
	val hubHost = remember(url) { Uri.parse(url).host }

	// The page cannot hide the app's own chrome, and CSS fullscreen inside a
	// WebView only fills the WebView - which is already the content area, so
	// nothing appears to happen. Let the page ask the app instead.
	val bridge = remember {
		object {
			@JavascriptInterface
			fun setFullscreen(on: Boolean) {
				onFullscreen(on)
			}
		}
	}

	DisposableEffect(Unit) {
		onDispose { onFullscreen(false) }
	}

	// A load failure is usually momentary - the phone switching from Wi-Fi to
	// mobile data, a tunnel re-keying. Retry on our own instead of leaving a
	// dead page and a Retry button for someone who is not looking.
	LaunchedEffect(error) {
		if (error != null) {
			delay(5_000)
			error = null
			reloadKey++
		}
	}

	Column(modifier.fillMaxSize()) {
		val failure = error
		if (failure != null) {
			Text(failure, color = Err, fontSize = 13.sp, modifier = Modifier.padding(vertical = 8.dp))
			Text("Retrying in a few seconds…", color = Muted, fontSize = 12.sp)
			Button(onClick = { error = null; reloadKey++ }) { Text("Retry now") }
			return@Column
		}

		key(url, reloadKey) {
			HubWebView(url, hubHost, bridge) { error = it }
		}
	}
}

@SuppressLint("SetJavaScriptEnabled", "JavascriptInterface")
@Composable
private fun HubWebView(url: String, hubHost: String?, bridge: Any, onError: (String) -> Unit) {
	AndroidView(
		modifier = Modifier.fillMaxSize(),
		// A WebView dropped from the screen keeps running its page - and with it
		// the Live socket - until it is destroyed. Without this every tier
		// switch left a stream running in the background, spending data.
		onRelease = { view ->
			view.stopLoading()
			view.loadUrl("about:blank")
			view.destroy()
		},
		factory = { context ->
			RemoterWebView(context).apply {
				layoutParams = ViewGroup.LayoutParams(
					ViewGroup.LayoutParams.MATCH_PARENT,
					ViewGroup.LayoutParams.MATCH_PARENT,
				)
				settings.javaScriptEnabled = true
				settings.domStorageEnabled = true
				settings.mediaPlaybackRequiresUserGesture = false
				settings.cacheMode = WebSettings.LOAD_NO_CACHE

				addJavascriptInterface(bridge, "RemoterHost")

				// Without a chrome client, confirm() silently returns false - and
				// the Access page asks before revoking a device.
				webChromeClient = WebChromeClient()
				webViewClient = object : WebViewClient() {
					override fun shouldOverrideUrlLoading(
						view: WebView?,
						request: WebResourceRequest?,
					): Boolean {
						// Pin the WebView to the hub; anything else is not ours.
						return request?.url?.host != hubHost
					}

					override fun onReceivedError(
						view: WebView?,
						request: WebResourceRequest?,
						err: WebResourceError?,
					) {
						if (request?.isForMainFrame == true) {
							onError("Cannot reach the hub. Is Tailscale on?")
						}
					}
				}
				loadUrl(url)
			}
		},
	)
}
