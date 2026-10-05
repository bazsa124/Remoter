package dev.remoter.node

import android.graphics.Bitmap
import android.os.Build
import android.util.Log
import fi.iki.elonen.NanoHTTPD
import fi.iki.elonen.NanoHTTPD.IHTTPSession
import fi.iki.elonen.NanoHTTPD.Response
import fi.iki.elonen.NanoHTTPD.newFixedLengthResponse
import fi.iki.elonen.NanoWSD
import org.json.JSONArray
import org.json.JSONObject
import java.io.ByteArrayInputStream
import java.io.ByteArrayOutputStream
import java.io.IOException
import java.security.MessageDigest
import kotlin.concurrent.thread
import kotlin.math.roundToInt

/** What the server needs from the service that runs it. */
interface NodeHost {
	val name: String
	val version: String
	val token: String
	fun allowed(ip: String): Boolean
	fun sessionBegin(controller: String, tier: String): Int
	fun sessionEnd(id: Int)
	fun glance(controller: String)
	fun sessions(): JSONArray
	fun power(): JSONObject?
	/** Keeps the screen on (and turns it on) until the returned function is called. */
	fun holdScreenOn(): () -> Unit

	val context: android.content.Context

	/** Shows the "Start now" prompt; the projection, or null if declined. */
	fun requestProjection(onResult: (android.media.projection.MediaProjection?) -> Unit)

	fun projectionEnded()
}

/**
 * The phone's node API - the same surface a Windows or Linux node serves, so the
 * hub relays to it without knowing it is a phone.
 *
 * Bound to the tailnet address only, and answering the hub's address only: the
 * hub has already authenticated the controller, checked the PIN and logged the
 * session before anything reaches here.
 */
class NodeServer(host: String, private val node: NodeHost) : NanoWSD(host, NodeSettings.PORT) {

	override fun serve(session: IHTTPSession): Response {
		if (!node.allowed(session.remoteIpAddress)) {
			Log.w(TAG, "refused ${session.remoteIpAddress} ${session.uri}")
			return plain(Response.Status.FORBIDDEN, "forbidden")
		}
		val presented = session.headers["authorization"].orEmpty().removePrefix("Bearer ")
		if (!MessageDigest.isEqual(presented.toByteArray(), node.token.toByteArray())) {
			return json(Response.Status.UNAUTHORIZED, JSONObject().put("error", "invalid or missing token"))
		}
		return super.serve(session) // WebSocket upgrade, or serveHttp
	}

	override fun serveHttp(session: IHTTPSession): Response = when (session.uri) {
		"/api/health" -> json(Response.Status.OK, health())
		"/api/monitors" -> monitors()
		"/api/screenshot" -> screenshot(session)
		"/api/arm", "/api/disarm" ->
			json(NOT_IMPLEMENTED, JSONObject().put("error", "a phone is not armed: it does not sleep the way a laptop does"))
		else -> json(Response.Status.NOT_FOUND, JSONObject().put("error", "no such endpoint"))
	}

	override fun openWebSocket(handshake: IHTTPSession): WebSocket = when (handshake.uri) {
		"/api/live" -> LiveSocket(handshake, node)
		else -> QuietSocket(handshake) // /api/stream: a phone has no jobs to report
	}

	private fun health(): JSONObject {
		val control = PhoneControl.instance
		val canSee = control != null && Build.VERSION.SDK_INT >= Build.VERSION_CODES.R
		val reason = when {
			Build.VERSION.SDK_INT < Build.VERSION_CODES.R -> "needs Android 11 or later"
			control == null -> "turn on \"Remoter remote control\" in the phone's Accessibility settings"
			else -> null
		}
		val tiers = JSONObject()
			.put("actions", false).put("actionsReason", "there are no actions on a phone")
			.put("console", false).put("consoleReason", "there is no shell on a phone")
			.put("glance", canSee).put("live", canSee)
		reason?.let { tiers.put("glanceReason", it) }
		return JSONObject()
			.put("version", node.version)
			.put("name", node.name)
			.put("mode", "android")
			.put("os", "android")
			.put("arch", Build.SUPPORTED_ABIS.firstOrNull().orEmpty())
			.put("elevated", false)
			.put("tiers", tiers)
			.put("arm", JSONObject().put("supported", false).put("armed", false))
			.put("sessions", node.sessions())
			.apply { node.power()?.let { put("power", it) } }
	}

	private fun monitors(): Response {
		val control = PhoneControl.instance
			?: return json(NOT_IMPLEMENTED, JSONObject().put("error", "accessibility service is off"))
		val size = control.screenSize()
		val list = JSONArray().put(
			JSONObject().put("index", 0).put("width", size.x).put("height", size.y).put("primary", true)
		)
		return newFixedLengthResponse(Response.Status.OK, "application/json", list.toString())
	}

	private fun screenshot(session: IHTTPSession): Response {
		val control = PhoneControl.instance
			?: return json(NOT_IMPLEMENTED, JSONObject().put("error", "accessibility service is off"))
		val q = session.parameters
		val quality = q["quality"]?.firstOrNull()?.toIntOrNull()?.coerceIn(1, 100) ?: 60
		val scale = q["scale"]?.firstOrNull()?.toDoubleOrNull()?.coerceIn(0.1, 1.0) ?: 0.5

		node.glance(controller(session))
		val release = node.holdScreenOn()
		try {
			val shot = control.screenshot()
				?: return json(
					Response.Status.SERVICE_UNAVAILABLE,
					JSONObject().put("error", "the phone refused the screenshot (secure screen, or too soon) - try again"),
				)
			val scaled = scaleOf(shot, scale)
			val buf = ByteArrayOutputStream()
			scaled.compress(Bitmap.CompressFormat.JPEG, quality, buf)
			val dims = "${scaled.width}x${scaled.height}"
			if (scaled !== shot) scaled.recycle()
			shot.recycle()
			val bytes = buf.toByteArray()
			return newFixedLengthResponse(Response.Status.OK, "image/jpeg", ByteArrayInputStream(bytes), bytes.size.toLong())
				.apply {
					addHeader("X-Remoter-Bytes", bytes.size.toString())
					addHeader("X-Remoter-Dimensions", dims)
					addHeader("Cache-Control", "no-store")
				}
		} finally {
			release()
		}
	}

	companion object {
		private const val TAG = "NodeServer"

		val NOT_IMPLEMENTED = object : Response.IStatus {
			override fun getRequestStatus() = 501
			override fun getDescription() = "501 Not Implemented"
		}

		fun json(status: Response.IStatus, body: JSONObject): Response {
			val text = body.toString()
			return newFixedLengthResponse(status, "application/json", text).apply {
				addHeader("X-Remoter-Bytes", text.toByteArray().size.toString())
			}
		}

		fun plain(status: Response.IStatus, text: String): Response =
			newFixedLengthResponse(status, NanoHTTPD.MIME_PLAINTEXT, text)

		fun controller(session: IHTTPSession): String =
			session.headers["x-remoter-controller"]?.takeIf { it.isNotBlank() } ?: "local"

		fun scaleOf(src: Bitmap, scale: Double): Bitmap {
			if (scale >= 0.999) return src
			val w = (src.width * scale).roundToInt().coerceAtLeast(16)
			val h = (src.height * scale).roundToInt().coerceAtLeast(16)
			return Bitmap.createScaledBitmap(src, w, h, true)
		}
	}
}

/** Holds a socket open and keeps it alive: the hub's job stream, which a phone never fills. */
private class QuietSocket(handshake: NanoHTTPD.IHTTPSession) : NanoWSD.WebSocket(handshake) {
	@Volatile
	private var open = true

	override fun onOpen() {
		thread(name = "quiet-ping", isDaemon = true) {
			while (open) {
				Thread.sleep(PING_EVERY_MS)
				try {
					if (open) ping(ByteArray(0))
				} catch (_: IOException) {
					open = false
				}
			}
		}
	}

	override fun onClose(code: NanoWSD.WebSocketFrame.CloseCode?, reason: String?, initiatedByRemote: Boolean) {
		open = false
	}

	override fun onMessage(message: NanoWSD.WebSocketFrame?) {}
	override fun onPong(pong: NanoWSD.WebSocketFrame?) {}
	override fun onException(exception: IOException?) {
		open = false
	}
}

const val PING_EVERY_MS = 20_000L

/**
 * One Live session: frames out as differenced tiles, taps and swipes in.
 *
 * The viewer is the same page that drives a PC, so its messages are PC-shaped:
 * a mouse click becomes a tap, the long-press "right click" a long press, wheel
 * notches swipes, and Windows key codes for Backspace, Enter and Escape map to
 * their phone equivalents. Phone-only buttons (Back, Home, Recents) arrive as
 * "nav", flicks as "swipe", and hold-then-move gestures as "drag".
 *
 * Frames come from accessibility screenshots - unattended, a few a second - or,
 * after the viewer asks for "smooth" and someone taps "Start now" on the phone,
 * from a screen recording at up to 10 fps. The viewer is told which, as a small
 * JSON text message beside the binary frames.
 */
private class LiveSocket(
	private val handshake: NanoHTTPD.IHTTPSession,
	private val node: NodeHost,
) : NanoWSD.WebSocket(handshake) {

	@Volatile private var open = true
	@Volatile private var fps = 2
	@Volatile private var quality = 35
	@Volatile private var scale = 0.4
	@Volatile private var forceKey = true
	@Volatile private var smooth: ProjectionSource? = null
	@Volatile private var asking = false
	private var lastX = 0.5f
	private var lastY = 0.5f

	override fun onOpen() {
		thread(name = "live", isDaemon = true) { stream() }
	}

	private fun stream() {
		val control = PhoneControl.instance
		if (control == null) {
			try {
				close(NanoWSD.WebSocketFrame.CloseCode.InternalServerError, "accessibility service is off", false)
			} catch (_: IOException) {
			}
			return
		}
		val screenshots = AccessibilitySource(control)
		val session = node.sessionBegin(NodeServer.controller(handshake), "live")
		val release = node.holdScreenOn()
		val coder = TileCoder()
		var frame = 0L
		var lastPing = System.currentTimeMillis()
		status(if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.Q) "off" else "unsupported")
		try {
			while (open) {
				val started = System.currentTimeMillis()
				val projected = smooth
				if (projected != null && projected.ended) endSmooth("stopped") // stopped from the status bar
				val source: FrameSource = smooth ?: screenshots

				val shot = source.grab(scale)
				if (shot != null) {
					val key = forceKey || frame % KEYFRAME_EVERY == 0L
					forceKey = false
					frame++
					val msg = coder.encode(shot, quality, key)
					shot.recycle()
					if (msg != null) {
						try {
							send(msg)
							coder.commit()
						} catch (e: IOException) {
							coder.discard()
							break
						}
					}
				}
				val now = System.currentTimeMillis()
				if (now - lastPing > PING_EVERY_MS) {
					lastPing = now
					try {
						ping(ByteArray(0))
					} catch (_: IOException) {
						break
					}
				}
				// Screenshots are rate-limited by Android, so asking faster only
				// collects refusals; a recording can go as fast as we encode.
				val rate = if (source === screenshots) fps.coerceIn(1, screenshots.maxFps)
				else maxOf(fps, SMOOTH_FPS).coerceAtMost(source.maxFps)
				val wait = 1000L / rate - (now - started)
				if (wait > 0) Thread.sleep(wait)
			}
		} catch (_: InterruptedException) {
		} finally {
			open = false
			endSmooth(null)
			release()
			node.sessionEnd(session)
		}
	}

	/** Asks the phone for a screen recording; someone there must tap "Start now". */
	private fun startSmooth() {
		if (smooth != null || asking) return
		if (Build.VERSION.SDK_INT < Build.VERSION_CODES.Q) {
			status("unsupported")
			return
		}
		asking = true
		status("requested")
		node.requestProjection { projection ->
			asking = false
			when {
				projection == null -> status("declined")
				!open -> { // the viewer left while the prompt was up
					runCatching { projection.stop() }
					node.projectionEnded()
				}
				else -> {
					smooth = ProjectionSource(node.context, projection)
					forceKey = true
					status("on")
				}
			}
		}
	}

	private fun endSmooth(reason: String?) {
		val s = smooth ?: return
		smooth = null
		s.close()
		node.projectionEnded()
		forceKey = true
		reason?.let { status(it) }
	}

	private fun status(state: String) {
		runCatching { send(JSONObject().put("type", "status").put("smooth", state).toString()) }
	}

	override fun onMessage(message: NanoWSD.WebSocketFrame) {
		val msg = runCatching { JSONObject(message.textPayload) }.getOrNull() ?: return
		val control = PhoneControl.instance ?: return
		val size = control.screenSize()
		fun px(v: Double, extent: Int) = (v.coerceIn(0.0, 1.0) * (extent - 1)).toFloat()

		when (msg.optString("type")) {
			"config" -> msg.optJSONObject("config")?.let { c ->
				fps = c.optInt("fps", fps).coerceIn(1, 30)
				quality = c.optInt("quality", quality).coerceIn(10, 90)
				val s = c.optDouble("scale", scale)
				if (s in 0.15..1.0) {
					if (s != scale) forceKey = true
					scale = s
				}
			}
			"resync" -> forceKey = true
			"smooth" -> if (msg.optBoolean("on")) startSmooth() else endSmooth("off")
			"move" -> {
				lastX = px(msg.optDouble("x"), size.x)
				lastY = px(msg.optDouble("y"), size.y)
			}
			"click" -> {
				lastX = px(msg.optDouble("x"), size.x)
				lastY = px(msg.optDouble("y"), size.y)
				if (!msg.optBoolean("down")) {
					val long = msg.optString("button") == "right"
					control.tap(lastX, lastY, if (long) 700 else 60)
				}
			}
			"swipe" -> control.swipe(
				px(msg.optDouble("x1"), size.x), px(msg.optDouble("y1"), size.y),
				px(msg.optDouble("x2"), size.x), px(msg.optDouble("y2"), size.y),
				msg.optLong("ms", 250),
			)
			"drag" -> {
				val raw = msg.optJSONArray("points") ?: return
				val points = (0 until raw.length()).mapNotNull { i ->
					raw.optJSONArray(i)?.let { p ->
						android.graphics.PointF(px(p.optDouble(0), size.x), px(p.optDouble(1), size.y))
					}
				}
				control.drag(points, msg.optLong("hold", 600), msg.optLong("ms", 400))
			}
			"scroll" -> {
				// One wheel notch is a quarter-screen swipe. Wheel away (positive)
				// scrolls content up, which on a phone is a finger moving down.
				val notches = msg.optInt("delta") / 120.0
				val distance = (size.y * 0.25 * notches).toFloat()
				val y1 = (lastY - distance / 2).coerceIn(1f, size.y - 2f)
				val y2 = (lastY + distance / 2).coerceIn(1f, size.y - 2f)
				control.swipe(lastX, y1, lastX, y2, 220)
			}
			"text" -> control.type(msg.optString("text"))
			"key" -> if (msg.optBoolean("down")) when (msg.optInt("key")) {
				VK_BACK -> control.backspace()
				VK_RETURN -> control.enter()
				VK_ESCAPE -> control.nav("back")
			}
			"nav" -> control.nav(msg.optString("action"))
		}
	}

	override fun onClose(code: NanoWSD.WebSocketFrame.CloseCode?, reason: String?, initiatedByRemote: Boolean) {
		open = false
	}

	override fun onPong(pong: NanoWSD.WebSocketFrame?) {}
	override fun onException(exception: IOException?) {
		open = false
	}

	companion object {
		const val SMOOTH_FPS = 8
		const val KEYFRAME_EVERY = 60L
		const val VK_BACK = 8
		const val VK_RETURN = 13
		const val VK_ESCAPE = 27
	}
}
