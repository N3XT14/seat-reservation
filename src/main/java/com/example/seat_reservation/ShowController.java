package com.example.seat_reservation;

import com.example.seat_reservation.dto.CreateShowRequest;
import com.example.seat_reservation.dto.ShowResponse;
import com.example.seat_reservation.exception.ForbiddenException;
import jakarta.servlet.http.HttpServletRequest;
import jakarta.validation.Valid;
import org.springframework.http.ResponseEntity;
import org.springframework.web.bind.annotation.*;

import java.util.List;

@RestController
public class ShowController {

    private final ShowService showService;

    public ShowController(ShowService showService) {
        this.showService = showService;
    }

    @PostMapping("/shows")
    public ResponseEntity<ShowResponse> createShow(@Valid @RequestBody CreateShowRequest req, HttpServletRequest request) {
        if (!"admin".equals(request.getAttribute("role"))) {
            throw new ForbiddenException();
        }

        int perUserLimit = req.perUserLimit() != null ? req.perUserLimit() : 4;

        ShowService.CreatedShow created = showService.create(
            req.name(), req.venue(), req.pricePaise(), perUserLimit, req.seats()
        );

        ShowCache show = created.show();
        List<ShowResponse.SeatItem> seatItems = req.seats().stream()
            .map(label -> new ShowResponse.SeatItem(label, "available"))
            .toList();

        return ResponseEntity.status(201).body(
            new ShowResponse(
                String.valueOf(created.showId()),
                show.name(), show.venue(), show.pricePaise(), show.perUserLimit(),
                req.seats().size(), req.seats().size(), 0, 0,
                seatItems
            )
        );
    }

    @GetMapping("/shows/{showId}")
    public ResponseEntity<ShowResponse> getShow(@PathVariable long showId) {
        return ResponseEntity.ok(showService.getShow(showId));
    }
}
