package dev.remoter.node

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.app.Service
import android.content.Context
import android.content.Intent
import android.content.pm.ServiceInfo
import android.media.projection.MediaProjection
import android.media.projection.MediaProjectionManager
import android.os.BatteryManager
import android.os.Build
import android.os.IBinder
import android.os.PowerManager
import android.util.Log
import androidx.core.app.NotificationCompat
import androidx.core.app.ServiceCompat
import dev.remoter.MainActivity
import dev.remoter.R
import dev.remoter.net.HubSettings
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.RequestBody.Companion.toRequestBody
import org.json.JSONArray
import org.json.JSONObject
import java.net.Inet4Address
import java.net.NetworkInterface
import java.time.Instant
import java.util.concurrent.Executors
import java.util.concurrent.ScheduledExecutorService
import java.util.concurrent.TimeUnit

/**
 * Keeps this phone reachable by the hub: the "very small service" of the design,
 * phone edition.
 *
 * Idle, it is a listener on the tailnet address and nothing else. Screenshots
 * and input happen only inside a session the hub relayed, and every session
 * shows in this service's notification - the phone's session indicator.
 */
class NodeService : Service(), NodeHost {

	private lateinit var settings: NodeSettings
	private lateinit var hubSettings: HubSettings
	private lateinit var worker: ScheduledExecutorService

	private var server: NodeServer? = null
	private var boundIp: String? = null

	private data class Session(val controller: String, val tier: String, val since: Instant)

	private val lock = Any()
	private val open = LinkedHashMap<Int, Session>()
	private val glances = HashMap<String, Long>()
	private var nextId = 0
	private var screenHolds = 0
	private var wakeLock: PowerManager.WakeLock? = null
	private var projecting = false
	@Volatile private var pendingProjection: ((MediaProjection?) -> Unit)? = null

	override fun onCreate() {
		super.onCreate()
		instance = this
		settings = NodeSettings(this)
		hubSettings = HubSettings(this)
		worker = Executors.newSingleThreadScheduledExecutor()
		createChannel()
	}

	override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
		if (intent?.action == ACTION_STOP || !settings.enabled) {
			stopSelf()
			return START_NOT_STICKY
		}
		foreground()
		// Tailscale can come up after us, or change address after a re-login:
		// check every 10 s and (re)bind when the address appears or changes.
		worker.scheduleWithFixedDelay({ runCatching { bind() } }, 0, 10, TimeUnit.SECONDS)
		worker.scheduleWithFixedDelay({ refreshIndicator() }, 15, 15, TimeUnit.SECONDS)
		return START_STICKY
	}

	override fun onDestroy() {
		if (instance === this) instance = null
		worker.shutdownNow()
		server?.stop()
		server = null
		synchronized(lock) {
			wakeLock?.takeIf { it.isHeld }?.release()
			wakeLock = null
		}
		super.onDestroy()
	}

	override fun onBind(intent: Intent?): IBinder? = null

	/**
	 * (Re)declares the foreground service, with the mediaProjection type only
	 * while a projection runs: Android 14 refuses getMediaProjection without it,
	 * and keeping it otherwise would claim a screen recording that is not
	 * happening.
	 */
	private fun foreground() {
		var type = 0
		if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.UPSIDE_DOWN_CAKE) type = ServiceInfo.FOREGROUND_SERVICE_TYPE_SPECIAL_USE
		if (projecting) type = type or ServiceInfo.FOREGROUND_SERVICE_TYPE_MEDIA_PROJECTION
		ServiceCompat.startForeground(this, NOTIFICATION_ID, notification(), type)
	}

	// --- smooth mode -----------------------------------------------------------

	/**
	 * Asks Android for a screen recording. The prompt appears on the phone;
	 * [onResult] gets the projection, or null if it was declined or could not be
	 * shown. Launched through the Accessibility service, which - unlike a
	 * background service - is allowed to open an activity.
	 */
	override val context: Context get() = this

	override fun requestProjection(onResult: (MediaProjection?) -> Unit) {
		pendingProjection?.invoke(null)
		pendingProjection = onResult
		val intent = Intent(this, ProjectionActivity::class.java)
			.addFlags(Intent.FLAG_ACTIVITY_NEW_TASK) // not NO_HISTORY: it would never receive the result
		val starter: Context = PhoneControl.instance ?: this
		runCatching { starter.startActivity(intent) }.onFailure {
			Log.w(TAG, "cannot show the projection prompt", it)
			pendingProjection = null
			onResult(null)
		}
	}

	/** Called by [ProjectionActivity] with the owner's answer. */
	fun projectionResult(resultCode: Int, data: Intent?) {
		val callback = pendingProjection ?: return
		pendingProjection = null
		if (resultCode != android.app.Activity.RESULT_OK || data == null) {
			callback(null)
			return
		}
		projecting = true
		foreground()
		val projection = runCatching {
			getSystemService(MediaProjectionManager::class.java).getMediaProjection(resultCode, data)
		}.onFailure { Log.w(TAG, "getMediaProjection failed", it) }.getOrNull()
		if (projection == null) projectionEnded()
		callback(projection)
	}

	/** The smooth session is over: drop the recording type again. */
	override fun projectionEnded() {
		if (!projecting) return
		projecting = false
		foreground()
	}

	private fun bind() {
		val ip = tailnetIp()
		val alive = server?.isAlive == true
		if (ip == boundIp && alive) return
		server?.stop()
		server = null
		boundIp = null
		if (ip == null) {
			Log.i(TAG, "waiting for a tailnet address")
			return
		}
		val s = NodeServer(ip, this)
		// Generous read timeout: WebSockets sit idle between frames, and the
		// pings every 20 s keep a live one inside it.
		s.start(60_000, true)
		server = s
		boundIp = ip
		Log.i(TAG, "listening on $ip:${NodeSettings.PORT}")
		Thread({ hello() }, "hello").start() // retries for up to 30 s; not on the bind loop
	}

	/** Tells the hub we are reachable, so it shows the phone as online at once. */
	private fun hello() {
		val url = hubSettings.hubUrl.trimEnd('/') + "/hub/hello"
		val client = OkHttpClient.Builder().callTimeout(5, TimeUnit.SECONDS).build()
		repeat(6) { attempt ->
			if (attempt > 0) Thread.sleep(5_000)
			val ok = runCatching {
				client.newCall(Request.Builder().url(url).post(ByteArray(0).toRequestBody()).build())
					.execute().use { it.code == 204 }
			}.getOrDefault(false)
			if (ok) return
		}
	}

	// --- NodeHost ----------------------------------------------------------------

	override val name: String get() = Build.MODEL
	override val version: String
		get() = runCatching { packageManager.getPackageInfo(packageName, 0).versionName }.getOrNull() ?: "?"
	override val token: String get() = settings.token

	override fun allowed(ip: String): Boolean {
		val hub = settings.hubAddr
		return (hub.isNotEmpty() && ip == hub) || ip == "127.0.0.1"
	}

	override fun sessionBegin(controller: String, tier: String): Int {
		val id = synchronized(lock) {
			val id = ++nextId
			open[id] = Session(controller, tier, Instant.now())
			id
		}
		refreshIndicator()
		return id
	}

	override fun sessionEnd(id: Int) {
		synchronized(lock) { open.remove(id) }
		refreshIndicator()
	}

	override fun glance(controller: String) {
		synchronized(lock) { glances[controller] = System.currentTimeMillis() }
		refreshIndicator()
	}

	override fun sessions(): JSONArray = JSONArray().apply {
		synchronized(lock) {
			open.values.forEach {
				put(JSONObject().put("controller", it.controller).put("tier", it.tier).put("since", it.since.toString()))
			}
			val now = System.currentTimeMillis()
			glances.forEach { (who, at) ->
				if (now - at < GLANCE_WINDOW_MS) {
					put(JSONObject().put("controller", who).put("tier", "glance").put("since", Instant.ofEpochMilli(at).toString()))
				}
			}
		}
	}

	override fun power(): JSONObject? {
		val bm = getSystemService(BatteryManager::class.java) ?: return null
		val percent = bm.getIntProperty(BatteryManager.BATTERY_PROPERTY_CAPACITY)
		return JSONObject().put("onAC", bm.isCharging).put("hasBattery", true).put("percent", percent)
	}

	/**
	 * Turns the screen on and keeps it on while someone is looking: a dark
	 * screen has nothing to capture. The deprecated full wake lock is still the
	 * only way an app can switch the screen on by itself.
	 */
	@Suppress("DEPRECATION")
	override fun holdScreenOn(): () -> Unit {
		synchronized(lock) {
			if (screenHolds++ == 0) {
				val pm = getSystemService(PowerManager::class.java)
				wakeLock = pm.newWakeLock(
					PowerManager.SCREEN_BRIGHT_WAKE_LOCK or PowerManager.ACQUIRE_CAUSES_WAKEUP,
					"remoter:session",
				).apply { acquire(30 * 60_000L) }
			}
		}
		var released = false
		return {
			synchronized(lock) {
				if (!released) {
					released = true
					if (--screenHolds == 0) {
						wakeLock?.takeIf { it.isHeld }?.release()
						wakeLock = null
					}
				}
			}
		}
	}

	// --- the indicator -------------------------------------------------------------

	private fun who(): List<String> = synchronized(lock) {
		val now = System.currentTimeMillis()
		glances.entries.removeAll { now - it.value >= GLANCE_WINDOW_MS }
		(open.values.map { "${it.controller} (${tierName(it.tier)})" } +
			glances.keys.map { "$it (Glance)" }).distinct()
	}

	private fun refreshIndicator() {
		getSystemService(NotificationManager::class.java).notify(NOTIFICATION_ID, notification())
	}

	private fun notification(): Notification {
		val connected = who()
		val open = PendingIntent.getActivity(
			this, 0, Intent(this, MainActivity::class.java),
			PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT,
		)
		val title = if (connected.isEmpty()) "This phone can be controlled through your hub"
		else "Remote session: " + connected.joinToString(", ")
		val text = when {
			connected.isNotEmpty() -> "Someone is viewing or using this phone through Remoter."
			boundIp == null -> "Waiting for Tailscale…"
			PhoneControl.instance == null -> "Turn on the Accessibility service to allow screen and input."
			else -> "Idle. Sessions show here."
		}
		return NotificationCompat.Builder(this, CHANNEL_ID)
			.setContentTitle(title)
			.setContentText(text)
			.setSmallIcon(android.R.drawable.ic_menu_view)
			.setContentIntent(open)
			.setOngoing(true)
			.setOnlyAlertOnce(true) // refreshed every 15 s during a session: never buzz for that
			.setPriority(if (connected.isEmpty()) NotificationCompat.PRIORITY_LOW else NotificationCompat.PRIORITY_HIGH)
			.build()
	}

	private fun createChannel() {
		val channel = NotificationChannel(
			CHANNEL_ID, getString(R.string.node_channel_name), NotificationManager.IMPORTANCE_LOW,
		).apply { description = getString(R.string.node_channel_desc) }
		getSystemService(NotificationManager::class.java).createNotificationChannel(channel)
	}

	companion object {
		private const val TAG = "NodeService"
		private const val CHANNEL_ID = "remoter.node"
		private const val NOTIFICATION_ID = 2
		private const val GLANCE_WINDOW_MS = 90_000L
		const val ACTION_STOP = "dev.remoter.node.STOP"

		/** The running service, for the projection prompt to report back to. */
		@Volatile
		var instance: NodeService? = null
			private set

		fun start(context: Context) {
			context.startForegroundService(Intent(context, NodeService::class.java))
		}

		fun stop(context: Context) {
			context.startService(Intent(context, NodeService::class.java).setAction(ACTION_STOP))
		}

		/** This phone's tailnet IPv4 (100.64.0.0/10), or null while Tailscale is down. */
		fun tailnetIp(): String? = runCatching {
			NetworkInterface.getNetworkInterfaces().toList()
				.filter { it.isUp }
				.flatMap { it.inetAddresses.toList() }
				.filterIsInstance<Inet4Address>()
				.firstOrNull { a ->
					val b = a.address
					(b[0].toInt() and 0xff) == 100 && (b[1].toInt() and 0xc0) == 0x40
				}?.hostAddress
		}.getOrNull()

		private fun tierName(t: String) = when (t) {
			"live" -> "Live"
			"glance" -> "Glance"
			else -> t
		}
	}
}
