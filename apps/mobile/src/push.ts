import * as Notifications from "expo-notifications";
import { Platform } from "react-native";
import { api } from "./api";

/**
 * Registers the device's Expo push token with the backend so high-severity
 * finding alerts can reach this device (doc 03 §3.10).
 *
 * Non-fatal: if push registration fails (no Expo project ID configured, network
 * error, etc.), the app continues normally.
 */
export async function registerPushToken(): Promise<void> {
  try {
    const token = await Notifications.getExpoPushTokenAsync();
    await api.post("/v1/push/register", {
      device_token: token.data,
      platform: Platform.OS,
    });
  } catch (e) {
    // dev builds without a configured Expo project or push permission fail here —
    // never block the app on it.
  }
}
