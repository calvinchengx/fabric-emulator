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

/**
 * An ABFS custom token provider that gets a Storage-audience token from
 * entra-emulator the way it expects: a v2 client-credentials POST with
 * {@code scope=<resource>/.default}. ABFS's built-in ClientCredsTokenProvider
 * sends AAD v1 {@code resource=}, which entra-emulator's v2 token endpoint
 * rejects — so this bridges the two without any change to the emulator.
 *
 * Config (per account, e.g. onelake.dfs.fabric.microsoft.com):
 *   fs.azure.account.auth.type.<acct>          = Custom
 *   fs.azure.account.oauth.provider.type.<acct> = com.calvinchengx.fabricemu.EntraTokenProvider
 *   fs.azure.emu.token.endpoint  = http://entra-emulator:8443/<tenant>/oauth2/v2.0/token
 *   fs.azure.emu.client.id       = <app id>
 *   fs.azure.emu.client.secret   = <secret>
 *   fs.azure.emu.scope           = https://storage.azure.com/.default
 */
public class EntraTokenProvider implements CustomTokenProviderAdaptee {
  private String endpoint, clientId, clientSecret, scope;
  private long expiresAtMs;

  @Override
  public void initialize(Configuration conf, String accountName) throws IOException {
    endpoint = req(conf, "fs.azure.emu.token.endpoint");
    clientId = req(conf, "fs.azure.emu.client.id");
    clientSecret = req(conf, "fs.azure.emu.client.secret");
    scope = conf.get("fs.azure.emu.scope", "https://storage.azure.com/.default");
  }

  @Override
  public String getAccessToken() throws IOException {
    String body = "grant_type=client_credentials"
        + "&client_id=" + enc(clientId)
        + "&client_secret=" + enc(clientSecret)
        + "&scope=" + enc(scope);
    HttpURLConnection c = (HttpURLConnection) new URL(endpoint).openConnection();
    c.setRequestMethod("POST");
    c.setDoOutput(true);
    c.setRequestProperty("Content-Type", "application/x-www-form-urlencoded");
    try (OutputStream os = c.getOutputStream()) {
      os.write(body.getBytes(StandardCharsets.UTF_8));
    }
    if (c.getResponseCode() != 200) {
      throw new IOException("entra token endpoint returned " + c.getResponseCode()
          + ": " + read(c.getErrorStream()));
    }
    String resp = read(c.getInputStream());
    expiresAtMs = System.currentTimeMillis() + 3_000_000L; // < the token's 1h
    return jsonString(resp, "access_token");
  }

  @Override
  public Date getExpiryTime() {
    return new Date(expiresAtMs);
  }

  private static String req(Configuration conf, String key) throws IOException {
    String v = conf.get(key);
    if (v == null || v.isEmpty()) {
      throw new IOException("missing required config " + key);
    }
    return v;
  }

  private static String enc(String s) {
    return URLEncoder.encode(s, StandardCharsets.UTF_8);
  }

  private static String read(java.io.InputStream in) throws IOException {
    if (in == null) {
      return "";
    }
    return new String(in.readAllBytes(), StandardCharsets.UTF_8);
  }

  // Minimal JSON string-field extractor — avoids a JSON dep on the classpath.
  private static String jsonString(String json, String field) throws IOException {
    String key = "\"" + field + "\"";
    int i = json.indexOf(key);
    if (i < 0) {
      throw new IOException("field " + field + " not in token response");
    }
    i = json.indexOf(':', i) + 1;
    i = json.indexOf('"', i) + 1;
    int j = json.indexOf('"', i);
    return json.substring(i, j);
  }
}
