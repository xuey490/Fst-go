// FST 移动端应用模块。
//
// 关键点：
//  - 只打包 arm64-v8a：Go 核心库由 gomobile 针对 android/arm64 编译，
//    覆盖当前全部在售 Android 手机；如需模拟器（x86_64）可在脚本里追加 target。
//  - libs/fstcore.aar 由 scripts/build-aar.sh 生成，不进版本库。
//  - 释放签名在 buildTypes.release 里用环境变量或本地 keystore 提供；
//    日常调试用 assembleDebug，AGP 会用自动生成的 debug key 签名，可直接安装。
plugins {
    id("com.android.application")
    id("org.jetbrains.kotlin.android")
    id("org.jetbrains.kotlin.plugin.compose")
    id("org.jetbrains.kotlin.plugin.serialization")
}

android {
    namespace = "com.fst.phone"
    compileSdk = 35

    defaultConfig {
        applicationId = "com.fst.phone"
        minSdk = 26
        targetSdk = 35
        versionCode = 7
        versionName = "1.2.0"
        ndk {
            abiFilters += "arm64-v8a"
        }
    }

    buildFeatures {
        compose = true
    }

    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }

    kotlinOptions {
        jvmTarget = "17"
    }

    buildTypes {
        release {
            isMinifyEnabled = false
            // 发布时用 -Pandroid.injected.signing.* 或自行配置 keystore；
            // 未配置时 assembleRelease 产出的 APK 未签名，不能直接安装。
            signingConfig = signingConfigs.findByName("debug")
        }
    }

    packaging {
        resources.excludes += setOf("/META-INF/{AL2.0,LGPL2.1}")
        // 关键：必须让原生库解压到 nativeLibraryDir。Go 核心是一个真正的可执行
        // 文件（伪装成 lib*.so 才能进 jniLibs），只有落盘后才允许 execve。
        // 默认的 useLegacyPackaging=false 会把 .so 留在 APK 里直接 mmap，路径不存在。
        jniLibs {
            useLegacyPackaging = true
        }
    }
}

dependencies {
    implementation("androidx.core:core-ktx:1.13.1")
    implementation("androidx.activity:activity-compose:1.9.3")
    implementation("androidx.lifecycle:lifecycle-runtime-compose:2.8.7")
    implementation("androidx.lifecycle:lifecycle-viewmodel-compose:2.8.7")

    implementation(platform("androidx.compose:compose-bom:2024.10.01"))
    implementation("androidx.compose.ui:ui")
    implementation("androidx.compose.foundation:foundation")
    implementation("androidx.compose.material3:material3")

    implementation("org.jetbrains.kotlinx:kotlinx-coroutines-android:1.9.0")
    implementation("org.jetbrains.kotlinx:kotlinx-serialization-json:1.7.3")

    debugImplementation("androidx.compose.ui:ui-tooling")
}
