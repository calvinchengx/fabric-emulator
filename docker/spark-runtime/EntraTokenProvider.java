package com.calvinchengx.fabricemu;

import java.io.IOException;
import java.io.OutputStream;
import java.net.HttpURLConnection;
import java.net.URL;
import java.net.URLEncoder;
import java.nio.charset.StandardCharsets;
import java.util.Date;
import org.apache.hadoop.conf.Configuration;
import org.apache.hadoop.fs.azurebfs.extensions.CustomTokenProviderAdaptee;

/** Bridges Hadoop ABFS client credentials to entra-emulator's v2 token API. */
public class EntraTokenProvider implements CustomTokenProviderAdaptee {
  private String endpoint;
  private String clientId;
  private String clientSecret;
  private String scope;
  private long expiresAtMs;

  @Override
  public void initialize(Configuration conf, String accountName) throws IOException {
    endpoint = required(conf, "fs.azure.emu.token.endpoint");
    clientId = required(conf, "fs.azure.emu.client.id");
    clientSecret = required(conf, "fs.azure.emu.client.secret");
    scope = conf.get("fs.azure.emu.scope", "https://storage.azure.com/.default");
  }

  @Override
  public String getAccessToken() throws IOException {
    String body = "grant_type=client_credentials"
        + "&client_id=" + encode(clientId)
        + "&client_secret=" + encode(clientSecret)
        + "&scope=" + encode(scope);
    HttpURLConnection connection = (HttpURLConnection) new URL(endpoint).openConnection();
    connection.setRequestMethod("POST");
    connection.setDoOutput(true);
    connection.setRequestProperty("Content-Type", "application/x-www-form-urlencoded");
    try (OutputStream output = connection.getOutputStream()) {
      output.write(body.getBytes(StandardCharsets.UTF_8));
    }
    if (connection.getResponseCode() != 200) {
      throw new IOException("entra token endpoint returned " + connection.getResponseCode()
          + ": " + read(connection.getErrorStream()));
    }
    String response = read(connection.getInputStream());
    expiresAtMs = System.currentTimeMillis() + 3_000_000L;
    return jsonString(response, "access_token");
  }

  @Override
  public Date getExpiryTime() {
    return new Date(expiresAtMs);
  }

  private static String required(Configuration conf, String key) throws IOException {
    String value = conf.get(key);
    if (value == null || value.isEmpty()) {
      throw new IOException("missing required config " + key);
    }
    return value;
  }

  private static String encode(String value) {
    return URLEncoder.encode(value, StandardCharsets.UTF_8);
  }

  private static String read(java.io.InputStream input) throws IOException {
    return input == null ? "" : new String(input.readAllBytes(), StandardCharsets.UTF_8);
  }

  private static String jsonString(String json, String field) throws IOException {
    String key = "\"" + field + "\"";
    int start = json.indexOf(key);
    if (start < 0) {
      throw new IOException("field " + field + " not in token response");
    }
    start = json.indexOf(':', start) + 1;
    start = json.indexOf('"', start) + 1;
    int end = json.indexOf('"', start);
    if (start == 0 || end < start) {
      throw new IOException("invalid token response");
    }
    return json.substring(start, end);
  }
}
