package com.example.seat_reservation;

import com.example.seat_reservation.dto.CancelResponse;
import jakarta.servlet.http.HttpServletRequest;
import org.springframework.http.ResponseEntity;
import org.springframework.web.bind.annotation.*;

@RestController
public class CancelController {

    private final CancelService cancelService;

    public CancelController(CancelService cancelService) {
        this.cancelService = cancelService;
    }

    @PostMapping("/reservations/{reservationId}/cancel")
    public ResponseEntity<CancelResponse> cancel(
            @PathVariable long reservationId,
            HttpServletRequest request) {

        String userId = (String) request.getAttribute("user_id");
        return ResponseEntity.ok(cancelService.cancel(reservationId, userId));
    }
}
