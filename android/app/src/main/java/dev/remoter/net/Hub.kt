package dev.remoter.net

import android.content.Context
import android.content.SharedPreferences
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import kotlinx.serialization.Serializable
import kotlinx.serialization.encodeToString
import kotlinx.serialization.json.Json
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.RequestBody.Companion.toRequestBody
import okhttp3.Response
import java.io.IOException
import java.util.concurrent.TimeUnit

/**
 * Which hub this app talks to, and which device on it is selected.
 *
 * There is no token any more: the hub recognises this phone by its Tailscale
 * identity, so nothing secret lives here. The stored PIN, when the owner opts
 * in, is in [PinVault] behind the fingerprint.
 */
class HubSettings(context: Context) {

	private val prefs: SharedPreferences =
		context.getSharedPreferences("remoter.hub", Context.MODE_PRIVATE)

	init {
		// Builds before the hub kept per-machine tokens in an encrypted store.
		// Those tokens are dead now - the hub holds every node's key - so drop
		// them rather than leaving secrets on disk that no longer open anything.
		context.deleteSharedPreferences("remoter.secure")
	}

	var hubUrl: String
		get() = prefs.getString(KEY_HUB, DEFAULT_HUB).orEmpty()
		set(value) = prefs.edit().putString(KEY_HUB, normalise(value)).apply()

	var activeDevice: String
		get() = prefs.getString(KEY_DEVICE, "").orEmpty()
		set(value) = prefs.edit().putString(KEY_DEVICE, value).apply()

	private fun normalise(raw: String): String {
		val t = raw.trim().trimEnd('/')
		return if (t.startsWith("https://") || t.startsWith("http://")) t else "https://$t"
	}

	companion object {
		// From local.properties at build time; empty means "ask the user".
		val DEFAULT_HUB: String = dev.remoter.BuildConfig.DEFAULT_HUB
		private const val KEY_HUB = "hub"
		private const val KEY_DEVICE = "device"
	}
}

@Serializable
private data class ErrorBody(
	val error: String? = null,
	val code: String? = null,
	val nonce: String? = null,
	val expiresIn: Int? = null,
)

@Serializable
private data class RunResponse(val jobId: String)

@Serializable
private data class NoncePayload(val nonce: String)

@Serializable
private data class PinPayload(val pin: String)

@Serializable
private data class ArmPayload(val hours: Double)

@Serializable
private data class SetPinPayload(val pin: String, val current: String? = null)

@Serializable
private data class EnrollPayload(val token: String, val port: Int, val version: String)

/** The hub's answer to a node enrolling. */
@Serializable
data class EnrollResult(
	val state: String,
	val id: String = "",
	val name: String = "",
	val hub: String = "",
	val hubAddr: String = "",
)

/**
 * HTTP surface of the hub, and of the selected device through it.
 *
 * Byte accounting lives here rather than in the UI: knowing what a tier costs is
 * the reason the tiers exist, so it is measured at the one place that sees every
 * response.
 */
class HubClient(private val settings: HubSettings) {

	val json = Json {
		ignoreUnknownKeys = true
		explicitNulls = false
		encodeDefaults = true
	}

	@Volatile
	var bytes: Long = 0L
		private set

	val http: OkHttpClient = OkHttpClient.Builder()
		.connectTimeout(10, TimeUnit.SECONDS)
		.readTimeout(30, TimeUnit.SECONDS)
		// The Tier 1 socket is long-lived; pings keep carrier NAT state alive.
		.pingInterval(30, TimeUnit.SECONDS)
		.retryOnConnectionFailure(true)
		.build()

	private val jsonMedia = "application/json".toMediaType()

	val hub: String get() = settings.hubUrl
	private val device: String get() = settings.activeDevice

	private fun hubPath(path: String) = hub + path
	private fun devicePath(path: String) = hub + "/d/" + device + path

	private fun count(response: Response) {
		response.header("X-Remoter-Bytes")?.toLongOrNull()?.let { bytes += it }
	}

	/** The hub's error codes become exceptions the UI can branch on. */
	private fun failure(body: String, code: Int): Exception {
		val parsed = runCatching { json.decodeFromString<ErrorBody>(body) }.getOrNull()
		val msg = parsed?.error ?: "HTTP $code"
		return when (parsed?.code) {
			"pending" -> PendingApproval(msg)
			"pin_required" -> PinNeeded(msg, unset = false)
			"pin_unset" -> PinNeeded(msg, unset = true)
			"pin_wrong" -> PinRejected(msg, locked = false)
			"pin_locked" -> PinRejected(msg, locked = true)
			else -> IOException(msg)
		}
	}

	private suspend fun call(request: Request): String = withContext(Dispatchers.IO) {
		http.newCall(request).execute().use { res ->
			count(res)
			val body = res.body?.string().orEmpty()
			if (!res.isSuccessful) throw failure(body, res.code)
			body
		}
	}

	private suspend inline fun <reified T> get(url: String): T =
		json.decodeFromString(call(Request.Builder().url(url).build()))

	private suspend fun post(url: String, payload: String): String =
		call(Request.Builder().url(url).post(payload.toRequestBody(jsonMedia)).build())

	// --- hub -------------------------------------------------------------------

	suspend fun me(): Me = get(hubPath("/hub/me"))

	suspend fun devices(): List<Device> = get(hubPath("/hub/devices"))

	suspend fun enterPin(pin: String) {
		post(hubPath("/hub/pin"), json.encodeToString(PinPayload(pin)))
	}

	/** Sets the first PIN, or replaces it given the current one. */
	suspend fun setPin(pin: String, current: String? = null) {
		call(
			Request.Builder().url(hubPath("/hub/pin"))
				.put(json.encodeToString(SetPinPayload(pin, current)).toRequestBody(jsonMedia))
				.build()
		)
	}

	/**
	 * Registers this phone as a target. The hub identifies it by its tailnet
	 * identity; it lands pending until the owner approves it.
	 */
	suspend fun enrollNode(token: String, port: Int, version: String): EnrollResult = json.decodeFromString(
		post(hubPath("/hub/enroll"), json.encodeToString(EnrollPayload(token, port, version)))
	)

	/** Throws [PinNeeded] unless the guarded tiers are unlocked right now. */
	suspend fun requirePin() {
		val pin = me().pin
		if (!pin.set) throw PinNeeded("Set a PIN to protect the screen and shell tiers", unset = true)
		if (!pin.granted) throw PinNeeded("Enter the PIN", unset = false)
	}

	// --- the selected device -----------------------------------------------------

	suspend fun actions(): List<ActionView> = get(devicePath("/api/actions"))

	suspend fun jobs(): List<Job> = get(devicePath("/api/jobs"))

	suspend fun jobDetail(id: String): JobDetail = get(devicePath("/api/jobs/$id"))

	/** Throws [ConfirmRequired] when the device wants the second step. */
	suspend fun run(id: String, nonce: String? = null): String = withContext(Dispatchers.IO) {
		val payload = if (nonce == null) "{}" else json.encodeToString(NoncePayload(nonce))
		val req = Request.Builder().url(devicePath("/api/actions/$id/run"))
			.post(payload.toRequestBody(jsonMedia)).build()
		http.newCall(req).execute().use { res ->
			count(res)
			val body = res.body?.string().orEmpty()
			if (res.code == 409) {
				val challenge = runCatching { json.decodeFromString<ErrorBody>(body) }.getOrNull()
				val issued = challenge?.nonce
				if (issued != null) throw ConfirmRequired(issued, challenge.expiresIn ?: 60)
			}
			if (!res.isSuccessful) throw failure(body, res.code)
			json.decodeFromString<RunResponse>(body).jobId
		}
	}

	suspend fun cancel(jobId: String) {
		call(Request.Builder().url(devicePath("/api/jobs/$jobId")).delete().build())
	}

	suspend fun arm(hours: Double) {
		post(devicePath("/api/arm"), json.encodeToString(ArmPayload(hours)))
	}

	suspend fun disarm() {
		post(devicePath("/api/disarm"), "{}")
	}

	/** A Tier 1.5 frame, as raw JPEG bytes. */
	suspend fun glance(monitor: Int, quality: Int, scale: Double): ByteArray =
		withContext(Dispatchers.IO) {
			val url = devicePath("/api/screenshot?monitor=$monitor&quality=$quality&scale=$scale")
			http.newCall(Request.Builder().url(url).build()).execute().use { res ->
				if (!res.isSuccessful) throw failure(res.body?.string().orEmpty(), res.code)
				val data = res.body?.bytes() ?: ByteArray(0)
				bytes += data.size
				data
			}
		}

	fun streamUrl(): String = devicePath("/api/stream").replaceFirst("http", "ws")

	/** The hub's own page for one tier, rendered without chrome by the WebView. */
	fun tierPage(tier: String): String = "$hub/?embed=1&tier=$tier&device=$device"

	fun accessPage(): String = "$hub/access"
}
