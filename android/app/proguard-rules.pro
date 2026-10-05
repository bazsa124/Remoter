# kotlinx.serialization keeps its generated serializers via annotations.
-keepattributes *Annotation*, InnerClasses
-dontnote kotlinx.serialization.**
-keepclassmembers class dev.remoter.** {
    *** Companion;
}
-keepclasseswithmembers class dev.remoter.** {
    kotlinx.serialization.KSerializer serializer(...);
}

# The WebView bridge is called by name from JavaScript; R8 must not rename it.
-keepclassmembers class * {
    @android.webkit.JavascriptInterface <methods>;
}
