package dev.remoter.node

import android.content.Context
import java.security.SecureRandom

/**
 * The phone's side of being a target: whether it is on, the token the hub uses
 * to reach it, and the hub's address - the only one allowed to connect.
 *
 * The token sits in app-private storage, which only this app (and root) can
 * read. It is the hub's key to this phone's screen; it never leaves the phone
 * except once, over TLS, at enrolment.
 */
class NodeSettings(context: Context) {

	private val prefs = context.applicationContext
		.getSharedPreferences("remoter.node", Context.MODE_PRIVATE)

	var enabled: Boolean
		get() = prefs.getBoolean(KEY_ENABLED, false)
		set(value) = prefs.edit().putBoolean(KEY_ENABLED, value).apply()

	/** Generated on first use; 32 random bytes as hex, like the other nodes. */
	val token: String
		get() {
			prefs.getString(KEY_TOKEN, null)?.let { return it }
			val bytes = ByteArray(32).also { SecureRandom().nextBytes(it) }
			val token = bytes.joinToString("") { "%02x".format(it) }
			prefs.edit().putString(KEY_TOKEN, token).apply()
			return token
		}

	/** The hub's tailnet address, learned at enrolment. */
	var hubAddr: String
		get() = prefs.getString(KEY_HUB_ADDR, "").orEmpty()
		set(value) = prefs.edit().putString(KEY_HUB_ADDR, value).apply()

	/** "pending" or "approved", as the hub last said. */
	var enrolment: String
		get() = prefs.getString(KEY_STATE, "").orEmpty()
		set(value) = prefs.edit().putString(KEY_STATE, value).apply()

	companion object {
		const val PORT = 8737
		private const val KEY_ENABLED = "enabled"
		private const val KEY_TOKEN = "token"
		private const val KEY_HUB_ADDR = "hubAddr"
		private const val KEY_STATE = "state"
	}
}
