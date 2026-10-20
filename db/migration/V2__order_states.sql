CREATE FUNCTION check_order_transition() RETURNS TRIGGER AS $$
BEGIN
    IF OLD.status = NEW.status THEN
        RETURN NEW;
    END IF;
    IF (OLD.status = 'CREATED' AND NEW.status IN ('PAYMENT_PENDING', 'CANCELED'))
       OR (OLD.status = 'PAYMENT_PENDING' AND NEW.status IN ('PAID', 'CANCELED'))
       OR (OLD.status = 'PAID' AND NEW.status = 'SHIPPED')
       OR (OLD.status = 'SHIPPED' AND NEW.status = 'COMPLETED') THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'invalid order state transition' USING ERRCODE = '23514', CONSTRAINT = 'order_state_transition';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER orders_state BEFORE UPDATE OF status ON orders
FOR EACH ROW EXECUTE FUNCTION check_order_transition();
