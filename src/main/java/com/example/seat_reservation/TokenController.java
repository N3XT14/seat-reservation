package com.example.seat_reservation;

import com.example.seat_reservation.dto.TokenRequest;
import com.example.seat_reservation.dto.TokenResponse;
import com.example.seat_reservation.exception.ForbiddenException;
import com.nimbusds.jose.*;
import com.nimbusds.jose.crypto.MACSigner;
import com.nimbusds.jwt.JWTClaimsSet;
import com.nimbusds.jwt.SignedJWT;
import jakarta.validation.Valid;
import org.springframework.beans.factory.annotation.Value;
import org.springframework.http.ResponseEntity;
import org.springframework.web.bind.annotation.*;

import java.security.MessageDigest;
import java.time.Duration;
import java.time.Instant;
import java.util.Date;

import static java.nio.charset.StandardCharsets.UTF_8;

// Demo identity provider: issues HS256 tokens that JwtFilter verifies.
// User tokens are open by design; admin tokens require X-Admin-Key.
@RestController
public class TokenController {

    private static final Duration TTL = Duration.ofHours(2);

    private final MACSigner signer;
    private final byte[] adminKey;

    public TokenController(@Value("${jwt.secret}") String jwtSecret,
                           @Value("${admin.key:}") String adminKey) throws KeyLengthException {
        this.signer = new MACSigner(jwtSecret.getBytes(UTF_8));
        this.adminKey = adminKey.getBytes(UTF_8);
    }

    @PostMapping("/auth/token")
    public ResponseEntity<TokenResponse> issue(
            @Valid @RequestBody TokenRequest req,
            @RequestHeader(value = "X-Admin-Key", required = false) String adminKeyHeader) throws JOSEException {

        String role = req.role() != null ? req.role() : "user";
        if ("admin".equals(role) && !adminKeyMatches(adminKeyHeader)) {
            throw new ForbiddenException();
        }

        Instant exp = Instant.now().plus(TTL);
        JWTClaimsSet claims = new JWTClaimsSet.Builder()
            .claim("user_id", req.userId())
            .claim("role", role)
            .issueTime(new Date())
            .expirationTime(Date.from(exp))
            .build();

        SignedJWT jwt = new SignedJWT(new JWSHeader(JWSAlgorithm.HS256), claims);
        jwt.sign(signer);

        return ResponseEntity.ok(new TokenResponse(jwt.serialize(), req.userId(), role, exp.getEpochSecond()));
    }

    // Constant-time compare; an unset admin key means admin tokens are never issued (fails closed).
    private boolean adminKeyMatches(String provided) {
        if (adminKey.length == 0 || provided == null) return false;
        return MessageDigest.isEqual(adminKey, provided.getBytes(UTF_8));
    }
}