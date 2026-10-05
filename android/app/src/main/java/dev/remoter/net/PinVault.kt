package dev.remoter.net

import android.content.Context
import android.os.Build
import android.security.keystore.KeyGenParameterSpec
import android.security.keystore.KeyProperties
import android.util.Base64
import androidx.biometric.BiometricManager
import androidx.biometric.BiometricManager.Authenticators.BIOMETRIC_STRONG
import androidx.biometric.BiometricPrompt
import androidx.core.content.ContextCompat
import androidx.fragment.app.FragmentActivity
import java.security.KeyStore
import javax.crypto.Cipher
import javax.crypto.KeyGenerator
import javax.crypto.SecretKey
import javax.crypto.spec.GCMParameterSpec

/**
 * Keeps the PIN behind the fingerprint, if the owner opts in.
 *
 * The PIN is encrypted with a Keystore key that refuses to work until a strong
 * biometric has just been presented - so the ciphertext in this app's storage is
 * useless on its own, even to someone holding the unlocked phone. Enrolling a
 * new fingerprint invalidates the key, and the stored PIN with it: a borrowed
 * phone cannot be taught a new finger and then unlock Remoter.
 *
 * The hub still checks the PIN every time. This only saves typing it.
 */
class PinVault(context: Context) {

	private val app = context.applicationContext
	private val prefs = app.getSharedPreferences("remoter.pinvault", Context.MODE_PRIVATE)

	/** Whether this phone has a strong biometric enrolled at all. */
	val available: Boolean
		get() = BiometricManager.from(app).canAuthenticate(BIOMETRIC_STRONG) ==
			BiometricManager.BIOMETRIC_SUCCESS

	/** Whether a PIN is stored. */
	val stored: Boolean get() = prefs.contains(KEY_DATA)

	private fun keystore(): KeyStore = KeyStore.getInstance(PROVIDER).apply { load(null) }

	private fun key(): SecretKey {
		(keystore().getKey(ALIAS, null) as? SecretKey)?.let { return it }
		val spec = KeyGenParameterSpec.Builder(
			ALIAS, KeyProperties.PURPOSE_ENCRYPT or KeyProperties.PURPOSE_DECRYPT,
		)
			.setBlockModes(KeyProperties.BLOCK_MODE_GCM)
			.setEncryptionPaddings(KeyProperties.ENCRYPTION_PADDING_NONE)
			.setUserAuthenticationRequired(true)
			.setInvalidatedByBiometricEnrollment(true)
			.apply {
				if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.R) {
					// Every use needs a fresh biometric - no grace window.
					setUserAuthenticationParameters(0, KeyProperties.AUTH_BIOMETRIC_STRONG)
				}
			}
			.build()
		return KeyGenerator.getInstance(KeyProperties.KEY_ALGORITHM_AES, PROVIDER)
			.apply { init(spec) }
			.generateKey()
	}

	/** Encrypts and stores [pin] behind a fingerprint prompt. */
	fun save(activity: FragmentActivity, pin: String, done: (ok: Boolean, error: String?) -> Unit) {
		val cipher = runCatching { encryptCipher() }.getOrElse {
			// An invalidated key (new fingerprint enrolled) cannot be reused.
			clear()
			runCatching { encryptCipher() }.getOrElse { return done(false, it.message) }
		}
		prompt(activity, "Remember PIN", "Your fingerprint will unlock it next time", cipher) { c, err ->
			if (c == null) return@prompt done(false, err)
			runCatching {
				val data = c.doFinal(pin.toByteArray(Charsets.UTF_8))
				prefs.edit()
					.putString(KEY_DATA, Base64.encodeToString(data, Base64.NO_WRAP))
					.putString(KEY_IV, Base64.encodeToString(c.iv, Base64.NO_WRAP))
					.apply()
			}.onSuccess { done(true, null) }.onFailure { done(false, it.message) }
		}
	}

	/** Releases the stored PIN after a fingerprint, or reports why not. */
	fun unlock(activity: FragmentActivity, done: (pin: String?, error: String?) -> Unit) {
		val data = prefs.getString(KEY_DATA, null)
		val iv = prefs.getString(KEY_IV, null)
		if (data == null || iv == null) return done(null, "No PIN stored")
		val cipher = runCatching {
			Cipher.getInstance(TRANSFORM).apply {
				init(Cipher.DECRYPT_MODE, key(), GCMParameterSpec(128, Base64.decode(iv, Base64.NO_WRAP)))
			}
		}.getOrElse {
			clear()
			return done(null, "Fingerprints changed since the PIN was saved - type it again")
		}
		prompt(activity, "Unlock Remoter", "Releases the PIN stored on this phone", cipher) { c, err ->
			if (c == null) return@prompt done(null, err)
			runCatching { String(c.doFinal(Base64.decode(data, Base64.NO_WRAP)), Charsets.UTF_8) }
				.onSuccess { done(it, null) }
				.onFailure { done(null, it.message) }
		}
	}

	/** Forgets the stored PIN and its key. */
	fun clear() {
		prefs.edit().clear().apply()
		runCatching { keystore().deleteEntry(ALIAS) }
	}

	private fun encryptCipher(): Cipher =
		Cipher.getInstance(TRANSFORM).apply { init(Cipher.ENCRYPT_MODE, key()) }

	private fun prompt(
		activity: FragmentActivity,
		title: String,
		subtitle: String,
		cipher: Cipher,
		result: (Cipher?, String?) -> Unit,
	) {
		val callback = object : BiometricPrompt.AuthenticationCallback() {
			override fun onAuthenticationSucceeded(r: BiometricPrompt.AuthenticationResult) {
				result(r.cryptoObject?.cipher, null)
			}

			override fun onAuthenticationError(code: Int, message: CharSequence) {
				result(null, message.toString())
			}
		}
		val info = BiometricPrompt.PromptInfo.Builder()
			.setTitle(title)
			.setSubtitle(subtitle)
			.setNegativeButtonText("Type PIN")
			.setAllowedAuthenticators(BIOMETRIC_STRONG)
			.build()
		BiometricPrompt(activity, ContextCompat.getMainExecutor(activity), callback)
			.authenticate(info, BiometricPrompt.CryptoObject(cipher))
	}

	private companion object {
		const val PROVIDER = "AndroidKeyStore"
		const val ALIAS = "remoter.pin"
		const val TRANSFORM = "AES/GCM/NoPadding"
		const val KEY_DATA = "data"
		const val KEY_IV = "iv"
	}
}
