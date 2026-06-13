// java-jar Deployer 集成测试制品:最小 HTTP 服务,带健康端点。
// 行为对齐 demoapp(go):APP_HEALTH=ok 时 /healthz 返回 200,否则 503,用于触发回滚。
// 监听 APP_ADDR(默认 :18081,避开 go 测试用的 18080)。版本/健康经环境变量注入。
import com.sun.net.httpserver.HttpServer;
import java.net.InetSocketAddress;
import java.nio.charset.StandardCharsets;

public class Main {
    public static void main(String[] args) throws Exception {
        String addr = envOr("APP_ADDR", ":18081");
        String version = envOr("APP_VERSION", "dev");
        String health = envOr("APP_HEALTH", "ok");
        int port = Integer.parseInt(addr.substring(addr.indexOf(':') + 1));

        HttpServer s = HttpServer.create(new InetSocketAddress(port), 0);
        s.createContext("/healthz", ex -> {
            int code = "ok".equals(health) ? 200 : 503;
            byte[] body = (health.equals("ok") ? "healthy" : "unhealthy").getBytes(StandardCharsets.UTF_8);
            ex.sendResponseHeaders(code, body.length);
            ex.getResponseBody().write(body);
            ex.close();
        });
        s.createContext("/", ex -> {
            byte[] body = ("demojava " + version).getBytes(StandardCharsets.UTF_8);
            ex.sendResponseHeaders(200, body.length);
            ex.getResponseBody().write(body);
            ex.close();
        });
        s.start();
        System.out.println("demojava " + version + " listening on " + addr);
    }

    static String envOr(String k, String def) {
        String v = System.getenv(k);
        return (v == null || v.isEmpty()) ? def : v;
    }
}
