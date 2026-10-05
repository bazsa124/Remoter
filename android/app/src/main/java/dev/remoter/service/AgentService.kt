package dev.remoter.service

import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.app.Service
import android.content.Context
import android.content.Intent
import android.os.Build
import android.os.IBinder
import android.util.Log
import androidx.core.app.NotificationCompat
import dev.remoter.MainActivity
import dev.remoter.R
import dev.remoter.net.AgentEvent
import dev.remoter.net.AgentState
import dev.remoter.net.HubClient
import dev.remoter.net.HubSettings
import okhttp3.Request
import okhttp3.Response
import okhttp3.WebSocket
import okhttp3.WebSocketListener
import kotlin.math.min
import kotlin.math.pow
import kotlin.random.Random

/**
 * Holds the Tier 1 socket to the selected device open, through the hub.
 *
 * The spec's invariant is that this connection stays up in every tier - mode
 * switching changes what is rendered, not what is connected. On Android that
 * means a foreground service: anything less is killed the moment the phone is
 * pocketed, which is precisely when a long job finishes. (Pushes for every
 * device come from the hub via ntfy either way; this socket is what makes the
 * job list live.)
 */
class AgentService : Service() {

	private lateinit var settings: HubSettings
	private lateinit var client: HubClient

	private var socket: WebSocket? = null
	private var attempt = 0
	private var stopping = false

	override fun onCreate() {
		super.onCreate()
		settings = HubSettings(this)
		client = HubClient(settings)
		createChannel()
	}

	override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
		if (intent?.action == ACTION_STOP) {
			stopSelf()
			return START_NOT_STICKY
		}

		startForeground(NOTIFICATION_ID, notification("Connecting…"))
		if (settings.activeDevice.isNotEmpty()) connect() else stopSelf()

		// Recreate after an OOM kill: the point of this service is persistence.
		return START_STICKY
	}

	override fun onDestroy() {
		stopping = true
		socket?.close(1000, "service stopping")
		socket = null
		AgentState.setConnected(false)
		super.onDestroy()
	}

	override fun onBind(intent: Intent?): IBinder? = null

	private fun connect() {
		if (stopping || settings.activeDevice.isEmpty()) return

		val request = Request.Builder().url(client.streamUrl()).build()
		socket = client.http.newWebSocket(request, object : WebSocketListener() {

			override fun onOpen(webSocket: WebSocket, response: Response) {
				attempt = 0
				AgentState.setConnected(true)
				update("Connected")
			}

			override fun onMessage(webSocket: WebSocket, text: String) {
				AgentState.addBytes(text.length.toLong())
				runCatching { client.json.decodeFromString<AgentEvent>(text) }
					.onSuccess { AgentState.apply(it) }
					.onFailure { Log.w(TAG, "bad event: ${it.message}") }
			}

			override fun onFailure(webSocket: WebSocket, t: Throwable, response: Response?) {
				AgentState.setConnected(false)
				// 403 means this phone was revoked on the hub; retrying cannot help.
				if (response?.code == 403) {
					update("Not allowed by the hub")
					return
				}
				scheduleReconnect(response?.let { "HTTP ${it.code}" } ?: t.message ?: "disconnected")
			}

			override fun onClosed(webSocket: WebSocket, code: Int, reason: String) {
				AgentState.setConnected(false)
				if (!stopping) scheduleReconnect("closed")
			}
		})
	}

	/**
	 * Exponential backoff with jitter. A phone on LTE drops this connection
	 * often, and a tight retry loop is how an app earns a reputation for eating
	 * battery.
	 */
	private fun scheduleReconnect(reason: String) {
		if (stopping) return
		val delay = min(MAX_BACKOFF_MS, (500.0 * 2.0.pow(attempt)).toLong())
		attempt++
		val jitter = Random.nextLong(0, (delay * 0.3).toLong().coerceAtLeast(1))
		update("Reconnecting… ($reason)")

		socket = null
		android.os.Handler(mainLooper).postDelayed({ connect() }, delay + jitter)
	}

	private fun createChannel() {
		if (Build.VERSION.SDK_INT < Build.VERSION_CODES.O) return
		val channel = NotificationChannel(
			CHANNEL_ID,
			getString(R.string.channel_name),
			// Low: this notification is a status line, not an interruption.
			NotificationManager.IMPORTANCE_LOW,
		).apply { description = getString(R.string.channel_desc) }

		getSystemService(NotificationManager::class.java).createNotificationChannel(channel)
	}

	private fun notification(status: String): android.app.Notification {
		val open = PendingIntent.getActivity(
			this, 0, Intent(this, MainActivity::class.java),
			PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT,
		)
		val stop = PendingIntent.getService(
			this, 1, Intent(this, AgentService::class.java).setAction(ACTION_STOP),
			PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT,
		)

		return NotificationCompat.Builder(this, CHANNEL_ID)
			.setContentTitle("Remoter")
			.setContentText(status)
			.setSmallIcon(android.R.drawable.stat_sys_upload_done)
			.setContentIntent(open)
			.addAction(0, "Disconnect", stop)
			.setOngoing(true)
			.setPriority(NotificationCompat.PRIORITY_LOW)
			.build()
	}

	private fun update(status: String) {
		getSystemService(NotificationManager::class.java)
			.notify(NOTIFICATION_ID, notification(status))
	}

	companion object {
		private const val TAG = "AgentService"
		private const val CHANNEL_ID = "remoter.connection"
		private const val NOTIFICATION_ID = 1
		private const val MAX_BACKOFF_MS = 30_000L
		const val ACTION_STOP = "dev.remoter.STOP"

		fun start(context: Context) {
			val intent = Intent(context, AgentService::class.java)
			context.startForegroundService(intent)
		}

		fun stop(context: Context) {
			context.startService(Intent(context, AgentService::class.java).setAction(ACTION_STOP))
		}
	}
}
