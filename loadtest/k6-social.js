// k6 load test for the SocialAI gateway. Run against `docker compose up`:
//
//   k6 run -e BASE=http://localhost -e VUS=50 loadtest/k6-social.js
//
// Scenarios:
//   browse  — search + home feed reads (cache and single-flight path)
//   hotpost — many users liking the same post (write-behind counter path)
//   upload  — posts with an Idempotency-Key, each retried once (idempotency path)
//
// The nginx gateway limits each client IP to 100 r/s (burst 200). One k6 machine is one IP,
// so above ~100 r/s you are measuring the rate limiter (503s), not the services. For a
// capacity run, raise `rate` in nginx/nginx.conf locally or run k6 from several machines.
//
// Watch alongside it in Prometheus/Grafana: http_request_duration_seconds (p95 per route),
// outbox_publish_lag_seconds, consumer_handle_seconds, consumer_dead_letters_total.
import http from "k6/http";
import { check, sleep } from "k6";
import { uuidv4 } from "https://jslib.k6.io/k6-utils/1.4.0/index.js";

const BASE = __ENV.BASE || "http://localhost";
const VUS = Number(__ENV.VUS || 20);

export const options = {
  scenarios: {
    browse: { executor: "constant-vus", vus: VUS, duration: "2m", exec: "browse" },
    hotpost: { executor: "constant-vus", vus: VUS, duration: "2m", exec: "hotpost", startTime: "10s" },
    upload: { executor: "constant-vus", vus: Math.max(1, Math.floor(VUS / 5)), duration: "2m", exec: "upload" },
  },
  thresholds: {
    "http_req_duration{route:search}": ["p(95)<300"],
    "http_req_duration{route:feed}": ["p(95)<300"],
    "http_req_duration{route:like}": ["p(95)<200"],
    http_req_failed: ["rate<0.01"],
  },
};

// 409 is an expected answer here (user already exists on signup, post already liked), not a
// failure, so it does not count toward http_req_failed.
http.setResponseCallback(http.expectedStatuses({ min: 200, max: 299 }, 409));

const IMAGE = open("./sample.png", "b");

function login(name) {
  const json = { "Content-Type": "application/json" };
  const creds = JSON.stringify({ user_id: name, password: "loadtest-pw" });
  http.post(`${BASE}/signup`, creds, { headers: json, tags: { route: "auth" } });
  const res = http.post(`${BASE}/signin`, creds, { headers: json, tags: { route: "auth" } });
  return { Authorization: `Bearer ${res.json("token")}` };
}

// One sign-in per VU, reused across iterations (bcrypt on every iteration would make the auth
// service the bottleneck of every scenario).
let vuAuth = null;
function session(name) {
  if (!vuAuth) vuAuth = login(name);
  return vuAuth;
}

export function setup() {
  const auth = login("lt_author");
  const res = http.post(`${BASE}/upload`, { message: "hot post", media_file: http.file(IMAGE, "hot.png") }, { headers: auth });
  return { hotPost: res.json("post_id") };
}

export function browse() {
  const auth = session(`lt_reader_${__VU}`);
  const s = http.get(`${BASE}/search?user_id=lt_author`, { headers: auth, tags: { route: "search" } });
  check(s, { "search 200": (r) => r.status === 200 });
  const f = http.get(`${BASE}/feed?limit=20`, { headers: auth, tags: { route: "feed" } });
  check(f, { "feed 200": (r) => r.status === 200 });
  sleep(0.2);
}

export function hotpost(data) {
  // A new user per like on purpose: every like must be distinct to load the counter path.
  const auth = login(`lt_fan_${__VU}_${__ITER}`);
  const r = http.post(`${BASE}/post/${data.hotPost}/like`, null, { headers: auth, tags: { route: "like" } });
  check(r, { "like 200/409": (x) => x.status === 200 || x.status === 409 });
}

export function upload() {
  const auth = session(`lt_uploader_${__VU}`);
  const key = uuidv4();
  const body = { message: `load test ${key}`, media_file: http.file(IMAGE, "p.png") };
  const headers = Object.assign({ "Idempotency-Key": key }, auth);
  const a = http.post(`${BASE}/upload`, body, { headers, tags: { route: "upload" } });
  const b = http.post(`${BASE}/upload`, body, { headers, tags: { route: "upload-retry" } });
  check(b, {
    "retry replays the same post": (r) => r.status === 201 && a.status === 201 && r.json("post_id") === a.json("post_id"),
  });
  sleep(1);
}
