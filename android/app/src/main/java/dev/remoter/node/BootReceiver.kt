package dev.remoter.node

import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent

/**
 * Restarts the listener after a reboot or an app update, if the owner turned
 * it on: a phone that only becomes reachable once someone opens the app is not
 * reachable when it matters.
 */
class BootReceiver : BroadcastReceiver() {
	override fun onReceive(context: Context, intent: Intent) {
		if (NodeSettings(context).enabled) NodeService.start(context)
	}
}
