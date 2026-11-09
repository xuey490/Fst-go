// FST 移动端 Gradle 设置。
//
// 仓库顺序说明：本机 maven.google.com 不可达，但 dl.google.com/dl/android/maven2
// 可用，gradle 的 google() 正是解析到该地址，因此无需额外镜像即可拉取 AndroidX。
pluginManagement {
    repositories {
        google()
        mavenCentral()
        gradlePluginPortal()
    }
}

dependencyResolutionManagement {
    repositoriesMode.set(RepositoriesMode.FAIL_ON_PROJECT_REPOS)
    repositories {
        google()
        mavenCentral()
    }
}

rootProject.name = "FST"
include(":app")
