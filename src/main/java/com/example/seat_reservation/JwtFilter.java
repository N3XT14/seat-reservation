package com.example.seat_reservation;

import com.nimbusds.jose.JWSAlgorithm;
import com.nimbusds.jose.crypto.MACVerifier;
import com.nimbusds.jwt.JWTClaimsSet;
import com.nimbusds.jwt.SignedJWT;
import jakarta.annotation.PostConstruct;
import jakarta.servlet.FilterChain;
import jakarta.servlet.ServletException;
import jakarta.servlet.http.HttpServletRequest;
import jakarta.servlet.http.HttpServletResponse;
import org.springframework.beans.factory.annotation.Value;
import org.springframework.stereotype.Component;
import org.springframework.web.filter.OncePerRequestFilter;

import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.util.Date;

@Component
public class JwtFilter extends OncePerRequestFilter {

    @Value("${jwt.secret}")
    private String jwtSecret;

    private byte[] secretBytes;

    @PostConstruct
    public void init() {
        secretBytes = jwtSecret.getBytes(StandardCharsets.UTF_8);
        if (secretBytes.length < 32) {
            throw new IllegalStateException("jwt.secret must be at least 32 bytes (currently " + secretBytes.length + ")");
        }
    }

    // Open paths
    @Override
    protected boolean shouldNotFilter(HttpServletRequest request) {
        String path = request.getServletPath();
        String method = request.getMethod();
        return path.equals("/auth/token")
            || path.startsWith("/healthz")
            || path.startsWith("/readyz")
            || path.startsWith("/actuator")
            || path.startsWith("/metrics")
            || ("GET".equalsIgnoreCase(method) && path.startsWith("/shows/"));
    }

    @Override
    protected void doFilterInternal(HttpServletRequest request, HttpServletResponse response, FilterChain chain) throws ServletException, IOException {
        String header = request.getHeader("Authorization");
        if (header == null || !header.startsWith("Bearer ")) {
            unauthorized(response);
            return;
        }

        String userId;
        String role;
        try {
            SignedJWT jwt = SignedJWT.parse(header.substring(7));

            // Reject anything other than HS256 and blocks alg:none and RS256 confusion attacks.
            if (!JWSAlgorithm.HS256.equals(jwt.getHeader().getAlgorithm())) {
                unauthorized(response);
                return;
            }

            if (!jwt.verify(new MACVerifier(secretBytes))) {
                unauthorized(response);
                return;
            }

            JWTClaimsSet claims = jwt.getJWTClaimsSet();

            // Require exp: every legitimate token comes from /auth/token, which always sets it.
            Date exp = claims.getExpirationTime();
            if (exp == null || exp.before(new Date())) {
                unauthorized(response);
                return;
            }

            // Accept user_id claim; fall back to sub.
            userId = claims.getStringClaim("user_id");
            if (userId == null) userId = claims.getSubject();
            if (userId == null || userId.isBlank()) {
                unauthorized(response);
                return;
            }

            role = claims.getStringClaim("role");

        } catch (Exception e) {
            // Covers parse errors, signature failures, and any Nimbus exception.
            unauthorized(response);
            return;
        }

        request.setAttribute("user_id", userId);
        request.setAttribute("role", role != null ? role : "");
        chain.doFilter(request, response);
    }

    private static void unauthorized(HttpServletResponse response) throws IOException {
        response.setStatus(401);
        response.setContentType("application/json");
        response.getWriter().write("{\"error\":\"unauthorized\"}");
    }
}
